package core

import (
	"context"
	"fmt"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// typingBeat — как часто обновляется действие чата «печатает»: оно гаснет само и в истории
// темы не остаётся.
const typingBeat = 4 * time.Second

// typingLimit — предел, после которого действие чата гаснет само даже у хода, который никто не
// закрыл. Обрыв соединения его гасит сразу, но ход может кончиться и без обрыва — молчанием
// средства сессии, — и тогда стучать в Bot API каждые четыре секунды некому и незачем.
const typingLimit = 5 * time.Minute

// agentMessage — дописанная реплика агента, как её видит владелец в терминале: в тему она уходит
// целиком и одним сообщением. Кусков на лету служба не принимает вовсе — пока реплика пишется, в
// теме стоит «печатает», и держат его границы хода. Правки уже отправленного сообщения на этом
// пути нет: сообщение темы появляется дописанным и больше не меняется.
//
// Вывод инструментов в этот поток не попадает, как не попадают его вход и мысли: отделяет их тот,
// кто их видит, то есть средство сессии; служба текст не разбирает и не угадывает. Разбор один и
// чужой службе: места и ссылки по записям `link_resolvers` настроек. Вывод их команд уходит
// отдельными сообщениями после реплики, а ответ на реплику их не ждёт.
//
// Нарезка остаётся одна: реплика длиннее предела сообщения Telegram режется по пределу тем же
// резаком, которым служба режет всякий длинный текст.
func (c *Core) agentMessage(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	if req.Text == "" {
		return protocol.Fail(req.RID, protocol.CodeBadRequest, "`agent.message` без `text`")
	}

	sess, fail := c.sessionInMode(conn, req)
	if fail != nil {
		return *fail
	}

	work := c.startResolving(sess, req.Text)

	// Сообщения сессии встают в тему цепочкой: реплика ждёт, пока ляжет всё от прошлого, а
	// следующее сообщение — её саму и вывод разворота её ссылок.
	done, err := c.enterChain(ctx, sess)
	if err != nil {
		c.abandon(work)

		return protocol.Fail(req.RID, protocol.CodeNetwork, err.Error())
	}

	resp := c.sendReply(ctx, conn, sess, req)
	if !resp.OK || work == nil {
		c.abandon(work)
		close(done)

		return resp
	}

	// Вывод разворота отправляется уже без звонящего: ход клиента ответа на реплику не ждёт
	// дольше, чем уходит сама реплика.
	go c.deliverResolved(sess, work, done)

	return resp
}

// sendReply отправляет реплику в тему сессии под замком темы.
func (c *Core) sendReply(
	ctx context.Context, conn *Conn, sess *Session, req protocol.Request,
) protocol.Response {
	c.topicMu.Lock()
	defer c.topicMu.Unlock()

	c.mu.Lock()

	// Поток ведёт соединение сессии, и только оно: иначе вытесненное соединение вело бы свой
	// поток рядом с настоящим и удваивало тему.
	if !c.holdsSession(sess, conn) {
		c.mu.Unlock()

		return protocol.Fail(req.RID, protocol.CodeBadRequest,
			"`agent.message` зовёт не соединение сессии")
	}

	c.mu.Unlock()

	ids, err := c.sendChunks(ctx, sess, req.Text)
	if err != nil {
		return failure(req.RID, err)
	}

	resp := protocol.Ok(req.RID)
	resp.MessageID = lastID(ids)
	resp.MessageIDs = ids

	return resp
}

// contextReport правит закреплённую строку на месте, но не чаще, чем разрешает настройка:
// правка сообщения служебных записей в теме не создаёт, но у Bot API свои лимиты.
//
// Режим проверяется здесь, и только здесь. Строка состояния живёт, пока владелец не у компьютера:
// со снятым режимом тема не трогается вовсе. Число служба помнит, но возврат режима шапку не
// правит: с запомненным числом заводится закреплённая строка новой темы, а строка уже живой темы
// догоняет процент первой границей хода после возврата. Средства сессии режима не знают и знать не
// должны — иначе одна и та же проверка жила бы в каждом клиенте по-своему.
func (c *Core) contextReport(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	c.mu.Lock()

	sess := c.session(sessionOf(conn, req))
	if sess == nil {
		c.mu.Unlock()

		return protocol.Fail(req.RID, protocol.CodeBadRequest, "`context.report` без сессии")
	}

	sess.percent = req.Percent
	topicID := sess.disk.TopicID
	name := sess.disk.TopicName
	inMode := sess.disk.Mode
	tooSoon := time.Since(sess.lastEdit) < c.cfg.ContextEditMin
	edit := inMode && topicID != 0 && !tooSoon

	if edit {
		sess.lastEdit = time.Now()
	}

	c.mu.Unlock()

	if edit {
		c.editPinned(ctx, sess, topicID, name, req.Percent)
	}

	return protocol.Ok(req.RID)
}

// say — единственный внешний интерфейс отправки. Звонящему не нужно знать ни пути состояния,
// ни устройства моста.
func (c *Core) say(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	if req.Text == "" {
		return protocol.Fail(req.RID, protocol.CodeBadRequest, "`say` без `text`")
	}

	switch protocol.SayKind(req.Kind) {
	case protocol.SayMessage, protocol.SayNotice:
	default:
		return protocol.Fail(req.RID, protocol.CodeBadRequest, "негодный `kind` у `say`")
	}

	c.mu.Lock()
	sess := c.session(sessionOf(conn, req))
	bound := sess != nil && sess.disk.TopicID != 0
	c.mu.Unlock()

	if !bound {
		c.topicMu.Lock()
		defer c.topicMu.Unlock()

		return c.sayLoose(ctx, req)
	}

	// Сказанное встаёт в тему после вывода разворота прошлой реплики, а не между ними.
	done, err := c.enterChain(ctx, sess)
	if err != nil {
		return protocol.Fail(req.RID, protocol.CodeNetwork, err.Error())
	}

	defer close(done)

	c.topicMu.Lock()
	defer c.topicMu.Unlock()

	ids, err := c.sendChunks(ctx, sess, req.Text)
	if err != nil {
		return failure(req.RID, err)
	}

	resp := protocol.Ok(req.RID)
	resp.MessageID = lastID(ids)
	resp.MessageIDs = ids

	return resp
}

// sayLoose отправляет в тему по умолчанию: звонящий без привязанной сессии темы не знает, а
// имя по умолчанию задано настройкой.
func (c *Core) sayLoose(ctx context.Context, req protocol.Request) protocol.Response {
	topicID, err := c.looseTopic(ctx, c.cfg.DefaultTopicName)
	if err != nil {
		return failure(req.RID, err)
	}

	ids, err := c.sendLoose(ctx, topicID, req.Text)
	if botapi.IsNoTopic(err) {
		c.mu.Lock()
		c.topics.Drop(topicID)
		c.mustSave(c.topics.Save(c.store))
		c.mu.Unlock()

		topicID, err = c.looseTopic(ctx, c.cfg.DefaultTopicName)
		if err != nil {
			return failure(req.RID, err)
		}

		ids, err = c.sendLoose(ctx, topicID, req.Text)
	}

	if err != nil {
		return failure(req.RID, errTopic(err))
	}

	resp := protocol.Ok(req.RID)
	resp.MessageID = lastID(ids)
	resp.MessageIDs = ids
	resp.TopicID = topicID

	return resp
}

// sendLoose отправляет текст в тему без привязки, нарезая его тем же пределом: у нарезанного
// наружу уходят все куски, а не последний.
func (c *Core) sendLoose(ctx context.Context, topicID int64, text string) ([]int, error) {
	chunks := botapi.Cut(text)
	ids := make([]int, 0, len(chunks))

	for _, part := range chunks {
		id, err := c.api.SendMessage(ctx, topicID, part)
		if err != nil {
			return ids, fmt.Errorf("кусок текста не отправлен: %w", err)
		}

		ids = append(ids, id)
	}

	return ids, nil
}

// looseTopic находит тему по имени в реестре или создаёт её, ничего не привязывая. Имя режется по
// пределу платформы на входе в реестр — так же, как имя привязанной темы.
func (c *Core) looseTopic(ctx context.Context, name string) (int64, error) {
	name = botapi.TopicName(name, "")

	c.mu.Lock()
	entry, ok := c.topics.ByNameLookup(name)
	c.mu.Unlock()

	if ok {
		return entry.ID, nil
	}

	topicID, err := c.api.CreateTopic(ctx, name)
	if err != nil {
		return 0, errTopic(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.topics.Put(store.Topic{ID: topicID, Name: name, InMode: false, PinnedID: 0, IconUnknown: false})
	c.mustSave(c.topics.Save(c.store))

	return topicID, nil
}

// turnEnd — извещение о конце хода: ответа оно не ждёт и ничего в тему не публикует. Ожидания
// хода у службы нет — конец хода просто кончается, сессия стоит, и будит её слово владельца
// доставкой и подачей промпта. Служба на этой границе гасит действие чата «печатает».
//
// Гасит она только свой ход. Извещения ответа не ждут и уходят у средства сессии без ожидания,
// поэтому порядок их прихода не обещан ничем: конец прошлого хода, обогнавший начало следующего,
// погасил бы «печатает» на весь следующий ход. Ход назван своим id — чужой конец проходит мимо и
// остаётся строкой в журнале; ход не назван вовсе — гасится текущий, другого адресата у такого
// конца нет.
func (c *Core) turnEnd(_ context.Context, conn *Conn, req protocol.Request) protocol.Response {
	c.mu.Lock()

	sess := c.session(sessionOf(conn, req))
	if sess == nil {
		c.mu.Unlock()

		return protocol.Fail(req.RID, protocol.CodeBadRequest, "`turn.end` без сессии")
	}

	held := sess.turn
	foreign := req.Turn != "" && held != "" && req.Turn != held
	sess.turnDone = req.Turn

	if !foreign {
		sess.stopTyping()
		sess.turn = ""
	}

	session := sess.disk.ID
	topicID := sess.disk.TopicID

	c.mu.Unlock()

	if foreign {
		c.journal(store.Entry{
			At: time.Now(), Event: "turn-end-foreign", Session: session, Outcome: held,
			Code: "", Text: req.Turn, TopicID: topicID,
		})
	}

	return protocol.Ok(req.RID)
}

// turnStart держит действие чата «печатает», пока идёт ход: живость сессии владелец видит им,
// и в истории темы оно не остаётся.
//
// Начало, опоздавшее за собственным концом, хода не начинает: гасить такое «печатает» было бы уже
// некому, и оно висело бы в теме до своего предела.
func (c *Core) turnStart(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	c.mu.Lock()

	sess := c.session(sessionOf(conn, req))
	if sess == nil {
		c.mu.Unlock()

		return protocol.Fail(req.RID, protocol.CodeBadRequest, "`turn.start` без сессии")
	}

	if req.Turn != "" && req.Turn == sess.turnDone {
		session := sess.disk.ID
		topicID := sess.disk.TopicID

		c.mu.Unlock()

		// Пропуск не бесследный: его видно строкой журнала. Настоящий случай здесь один —
		// переставленные извещения одного средства сессии, — и если он окажется другим, причина
		// будет названа, а не замолчана.
		c.journal(store.Entry{
			At: time.Now(), Event: "turn-start-stale", Session: session, Outcome: "",
			Code: "", Text: req.Turn, TopicID: topicID,
		})

		return protocol.Ok(req.RID)
	}

	topicID := sess.disk.TopicID
	inMode := sess.disk.Mode
	sess.stopTyping()
	sess.turn = req.Turn

	if topicID == 0 || !inMode {
		c.mu.Unlock()

		return protocol.Ok(req.RID)
	}

	// Действие чата держится до конца хода и переживает закрытие соединения звонящего: отмена
	// у него своя, от хода, а не от запроса. Сверху оно ограничено сроком — ход, кончившийся
	// обрывом, его не держит.
	typingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), typingLimit)
	sess.typingStop = cancel

	c.mu.Unlock()

	go c.holdTyping(typingCtx, topicID)

	return protocol.Ok(req.RID)
}

// holdTyping обновляет действие чата, пока ход не кончился.
func (c *Core) holdTyping(ctx context.Context, topicID int64) {
	ticker := time.NewTicker(typingBeat)
	defer ticker.Stop()

	for {
		err := c.api.SendChatAction(ctx, topicID, "typing")
		if err != nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
