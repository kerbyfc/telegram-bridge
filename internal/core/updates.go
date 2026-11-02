package core

import (
	"context"
	"strings"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// Команды самой службы в теме. Имён и смысла остальных команд служба не знает: всё прочее,
// начинающееся со слэша, уезжает моду как команда клиента. Команды состояния среди них нет: она
// печатала реестр ожиданий, которого больше нет.
const (
	cmdAway = "/away"
	cmdHere = "/here"
)

// Dispatch разбирает одно обновление опроса.
//
// Обновления своего авторства служба не разбирает вовсе — это граница входа, а не условие на
// пути разбора. Своё сообщение приходит обратно тем же опросом; службе оно не говорит ничего
// нового, потому что она сама его и отправила.
func (c *Core) Dispatch(ctx context.Context, upd botapi.Update) {
	if upd.FromSelf() || upd.Message == nil {
		return
	}

	// Правка темы — служебное сообщение, а не слово владельца: текста у неё нет, и уезжает она в
	// реестр тем, а не сессии.
	if upd.Message.TopicEdit != nil {
		c.topicEdited(upd.Message)

		return
	}

	c.handleMessage(ctx, upd.Message)
}

// topicEdited подхватывает правку темы, сделанную мимо службы, в реестр тем: значок, сменённый
// руками, расходился бы с записью реестра, и следующее переключение режима пропускалось бы как
// «уже так». Снятый значок — известное состояние «не в режиме». Поставленный — неизвестное: какой
// именно значок стоит, по id эмодзи ядро не знает (значки знает клиент Bot API), поэтому следующее
// переключение уходит платформе в любую сторону, а «уже так» клиент считает успехом.
//
// Правка без значка реестра не трогает, тема вне реестра — не тема службы. Кто правил, неважно:
// значок на платформе один, чей бы рукой он ни стоял. Повтор той же правки ничего не портит —
// запись приходит в то же состояние.
func (c *Core) topicEdited(msg *botapi.Message) {
	if !msg.TopicEdit.IconChanged {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.topics.ByIDLookup(msg.TopicID)
	if !ok {
		return
	}

	outcome := "icon-removed"

	if msg.TopicEdit.Icon == "" {
		entry.InMode = false
		entry.IconUnknown = false
	} else {
		entry.IconUnknown = true
		outcome = "icon-unknown"
	}

	c.topics.Put(entry)
	c.mustSave(c.topics.Save(c.store))

	c.journal(store.Entry{
		At: time.Now(), Event: "topic-edited", Session: "", Outcome: outcome,
		Code: "", Text: msg.TopicEdit.Icon, TopicID: msg.TopicID,
	})
}

// handleMessage разбирает сообщение в теме: сначала адресат, потом подлинность, потом текст, потом
// доставка. Порядок именно такой: бот получает обновления всех тем супергруппы, и говорить в теме,
// которую служба не ведёт, она не вправе.
func (c *Core) handleMessage(ctx context.Context, msg *botapi.Message) {
	sess, topic, ok := c.topicSession(msg.TopicID)
	if !ok {
		return
	}

	if !c.fromOwner(msg) {
		return
	}

	if !c.hasText(msg) {
		return
	}

	command, args, isOwn := ownCommand(msg.Text)
	if isOwn {
		c.topicCommand(ctx, sess, topic, msg.ID, command, args)

		return
	}

	if sess == nil {
		c.tell(ctx, msg.TopicID, "У этой темы нет сессии: сообщение никому не адресовано.")
		c.journal(store.Entry{
			At: time.Now(), Event: "message-without-session", Session: "",
			Outcome: "", Code: "", Text: "", TopicID: msg.TopicID,
		})

		return
	}

	// Путь у слова владельца один — доставка соединению сессии. Выбирать между путями службе не
	// приходится: ожидания хода нет, вопросов как сущности нет, а реплай только называется в строке
	// доставки.
	rec := c.enqueue(sess, recordInput{
		delivery:  deliveryKindOf(msg.Text),
		text:      msg.Text,
		author:    protocol.Author{ID: msg.From.ID, Name: msg.From.Name, Kind: protocol.AuthorHuman},
		messageID: msg.ID,
		at:        msg.At,
		reply:     msg.ReplyTo,
	})
	c.mark(ctx, sess, rec.MessageID, rec.TopicID, botapi.MarkReceived)
	c.announceUnreachable(ctx, sess, rec)
}

// announceUnreachable отвечает в тему, что сессии этой темы в режиме нет: слово владельца ей не
// адресовано вовсе. Само слово при этом придержано — у агента его нет, и владелец читает это
// отметкой на своём сообщении.
func (c *Core) announceUnreachable(ctx context.Context, sess *Session, rec *store.Record) {
	c.mu.Lock()
	dead := sess.state(time.Now(), c.cfg, c.readyAt) == lifeDead
	inMode := sess.disk.Mode
	topicID := sess.disk.TopicID
	id := sess.disk.ID

	c.mu.Unlock()

	if inMode && !dead {
		return
	}

	c.hold(ctx, sess, rec)
	c.tell(ctx, topicID, "Сессии этой темы в режиме нет.")
	c.journal(store.Entry{
		At: time.Now(), Event: "no-session-in-mode", Session: id, Outcome: "",
		Code: string(protocol.CodeNoMode), Text: "", TopicID: topicID,
	})
}

// fromOwner держит гейт по id владельца. Автора отброшенного сообщения служба в теме не называет
// и вообще о нём не говорит: в группе должен быть только владелец, и чужое сообщение для темы
// просто невидимо. След остаётся строкой журнала.
func (c *Core) fromOwner(msg *botapi.Message) bool {
	if msg.From == nil {
		c.journal(store.Entry{
			At: time.Now(), Event: "message-dropped", Session: "", Outcome: "anonymous",
			Code: "", Text: "", TopicID: msg.TopicID,
		})

		return false
	}

	if msg.From.ID != c.cfg.OwnerID {
		c.journal(store.Entry{
			At: time.Now(), Event: "message-dropped", Session: "", Outcome: "not-owner",
			Code: "", Text: msg.From.Name, TopicID: msg.TopicID,
		})

		return false
	}

	return true
}

// hasText держит вход по тексту: в теме владелец и агент говорят только текстом. Сообщение
// владельца без текста — стикер, фото, служебное «тема закрыта» или «открыта» после ручной правки —
// не вход: в ящик оно не ложится и сессии не отдаётся, а мод пустую доставку не принял бы, и запись
// висела бы в ящике вечно. Строк в тему про это нет; след — строка журнала, как у анонимного слова.
// Правка темы сюда не доходит: она идёт своим путём, в реестр тем.
func (c *Core) hasText(msg *botapi.Message) bool {
	if msg.Text != "" {
		return true
	}

	c.journal(store.Entry{
		At: time.Now(), Event: "message-dropped", Session: "", Outcome: "no-text",
		Code: "", Text: "", TopicID: msg.TopicID,
	})

	return false
}

// topicCommand исполняет команду самой службы, набранную в теме. Команда темы адресована её
// единственной сессии, а не всем привязкам. Вывода команды в теме нет: неудачу владелец читает
// отметкой «придержано» на своём сообщении — той же, что у отказанной команды клиента, — а
// причина уходит в журнал службы.
func (c *Core) topicCommand(
	ctx context.Context, sess *Session, topic store.Topic, messageID int, command, args string,
) {
	if sess == nil {
		c.tell(ctx, topic.ID, "У этой темы нет сессии: командой управлять некем.")

		return
	}

	// Живого соединения команда темы не требует: режим — свойство темы и её сессии, состояние
	// ложится на запись темы, а сессия застаёт его при подключении. Из темы режим включается
	// всегда, в том числе у сессии, которую служба уже похоронила.
	switch command {
	case cmdAway:
		// Имя темы аргументом — то же, что аргумент `mode.on`: пусто — остаётся прежнее имя.
		_, _, _, err := c.enableMode(ctx, sess, args)
		if err != nil {
			c.journal(store.Entry{
				At: time.Now(), Event: "away-refused", Session: sess.disk.ID, Outcome: "",
				Code: string(refusalOf(err).Code), Text: err.Error(), TopicID: topic.ID,
			})
			c.mark(ctx, sess, messageID, topic.ID, botapi.MarkHeld)
		}
	case cmdHere:
		c.disableMode(ctx, sess)
	default:
		panic("команда службы вне перечня: " + command)
	}
}

// topicSession находит тему в реестре и привязанную к ней сессию.
func (c *Core) topicSession(topicID int64) (*Session, store.Topic, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	topic, ok := c.topics.ByIDLookup(topicID)
	if !ok {
		c.journal(store.Entry{
			At: time.Now(), Event: "unknown-topic", Session: "", Outcome: "",
			Code: "", Text: "", TopicID: topicID,
		})

		return nil, store.Topic{ID: 0, Name: "", InMode: false, PinnedID: 0, IconUnknown: false}, false
	}

	return c.sessionByTopic(topicID), topic, true
}

// tell говорит владельцу строкой прямо в тему, из которой пришло обновление: тема заведомо
// существует, лечить её тут нечего.
func (c *Core) tell(ctx context.Context, topicID int64, text string) {
	if topicID == 0 {
		return
	}

	_, err := c.api.SendMessage(ctx, topicID, text)
	if err != nil {
		c.journal(store.Entry{
			At: time.Now(), Event: "tell-failed", Session: "", Outcome: "",
			Code: string(botapi.Code(err)), Text: err.Error(), TopicID: topicID,
		})
	}
}

// ownCommand отличает команду самой службы от всего прочего и отдаёт её аргумент. Имя команды —
// первое слово без суффикса `@<бот>`: в группе с несколькими ботами клиент Telegram подставляет
// имя бота сам, а меню команд — всегда, и точное равенство всей строки такую команду пропускало
// бы моду как команду клиента.
func ownCommand(text string) (string, string, bool) {
	head, args, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, _, _ := strings.Cut(head, "@")
	name = strings.ToLower(name)

	switch name {
	case cmdAway, cmdHere:
		return name, strings.TrimSpace(args), true
	}

	return "", "", false
}

// deliveryKindOf отличает команду клиента от слова владельца: имён и смысла команд служба не
// знает, всё прочее со слэша уезжает моду командой.
func deliveryKindOf(text string) protocol.DeliveryKind {
	if strings.HasPrefix(text, "/") {
		return protocol.DeliveryClientCmd
	}

	return protocol.DeliveryOwnerMessage
}
