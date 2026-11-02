package core

import (
	"context"
	"fmt"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// topicWork — вид работы с темой. Лечение пропавшей темы само ставит ей значок и строку
// состояния, поэтому значок после лечения второй раз не ставится.
type topicWork int

const (
	// workOther — работа, которую лечение не делает: отправка, переименование, реакция.
	workOther topicWork = iota
	// workIcon — переключение значка темы.
	workIcon
)

// modeOn включает режим: находит тему по имени в реестре или создаёт её, ставит значок,
// проверяет закреплённую строку и отвечает. Приветствий и сообщений «на связи» нет ни одного.
func (c *Core) modeOn(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	c.mu.Lock()
	sess := c.session(sessionOf(conn, req))
	c.mu.Unlock()

	if sess == nil {
		// Сессия, которой служба не знает, — сессия без подключённого мода, то есть поломка
		// установки: честный ответ на такой вызов — отказ с названной причиной.
		return protocol.Fail(req.RID, protocol.CodeBadRequest,
			"`mode.on` для сессии, которой служба не знает: её мод не подключён")
	}

	topicID, topicName, created, err := c.enableMode(ctx, sess, req.Name)
	if err != nil {
		return failure(req.RID, err)
	}

	resp := protocol.Ok(req.RID)
	resp.TopicID = topicID
	resp.TopicName = topicName
	resp.Created = created

	return resp
}

// enableMode включает режим сессии: привязка к теме, значок и закреплённая строка. Зовётся и
// операцией протокола, и командой владельца из темы.
func (c *Core) enableMode(ctx context.Context, sess *Session, requested string) (int64, string, bool, error) {
	c.topicMu.Lock()
	defer c.topicMu.Unlock()

	c.mu.Lock()

	// Имя режется по пределу платформы здесь, на входе в реестр: реестр держит ровно то имя, что
	// держит Telegram, и закреплённая строка, собранная из него, за предел сообщения не уходит.
	name := botapi.TopicName(pickName(requested, sess.disk.TopicName, c.cfg.DefaultTopicName), "")
	bound := sess.disk.TopicID != 0
	oldName := sess.disk.TopicName

	c.mu.Unlock()

	created := false

	var err error

	if bound {
		err = c.renameBound(ctx, sess, oldName, name)
	} else {
		created, err = c.bindTopic(ctx, sess, name)
	}

	if err != nil {
		return 0, "", false, err
	}

	c.mu.Lock()

	sess.disk.Mode = true
	topicID := sess.disk.TopicID
	topicName := sess.disk.TopicName
	c.mustSave(sess.disk.Save(c.store))
	c.mu.Unlock()

	c.setTopicMode(ctx, sess, true)
	c.ensurePinned(ctx, sess, topicID, topicName)
	// Имя темы и признак режима уезжают в статусную строку: своего состояния она не читает, и
	// режим виден владельцу ровно по этому вкладу.
	c.contribute(sess)
	// Слово владельца, дождавшееся в ящике возврата режима, уезжает моду сразу.
	c.wakePending(sess)

	return topicID, topicName, created, nil
}

// modeOff снимает режим: значок темы возвращается в «не в режиме», ожидание этой сессии
// снимается, в тему не пишется ничего. Привязка сессии к теме сохраняется.
func (c *Core) modeOff(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	c.mu.Lock()
	sess := c.session(sessionOf(conn, req))
	c.mu.Unlock()

	if sess == nil {
		return protocol.Fail(req.RID, protocol.CodeBadRequest, "`mode.off` без сессии")
	}

	resp := protocol.Ok(req.RID)
	resp.TopicID = c.disableMode(ctx, sess)

	return resp
}

// disableMode снимает режим сессии и отдаёт id её темы. Зовётся и операцией протокола, и
// командой владельца из темы.
func (c *Core) disableMode(ctx context.Context, sess *Session) int64 {
	c.mu.Lock()

	sess.disk.Mode = false
	topicID := sess.disk.TopicID
	topicName := sess.disk.TopicName
	// Со снятым режимом слово из ящика сессии не адресовано вовсе: что бы с ним ни было до
	// этого, у агента его теперь нет.
	held := make([]*store.Record, len(sess.inbox))
	copy(held, sess.inbox)
	c.mustSave(sess.disk.Save(c.store))
	c.mu.Unlock()

	c.hold(ctx, sess, held...)

	// Вклад остаётся на месте с пустым списком, а результат зонда убирается: «поставщик жив,
	// показывать нечего». Снятый режим обязан пропасть с экрана сразу, а не по сроку годности.
	c.contribute(sess)

	if topicID != 0 {
		c.setTopicMode(ctx, sess, false)
		c.editPinned(ctx, sess, topicID, topicName, 0)
	}

	return topicID
}

// renameBound переименовывает тему привязанной сессии. Новая тема на новое название не
// заводится: подмену темы владелец не видит никак, а переименование видит служебной строкой.
// Повторный вызов переименования тем же именем запрещён — Telegram отбивает его ошибкой.
func (c *Core) renameBound(ctx context.Context, sess *Session, oldName, name string) error {
	// Имя, которое в реестре уже держит другая тема, занять нельзя: ключ реестра — имя, и
	// вторая запись затёрла бы первую, оставив живую тему без записи. Поэтому имя, занятое
	// другой темой, получает наименьший свободный номерной суффикс — то же правило, что у
	// привязки, только свободным здесь считается имя, которого в реестре нет вовсе.
	fresh := c.freeName(name, sess.disk.TopicID)

	// С прежним именем сверяется имя после подбора суффикса, а не запрошенное: сессия, которая уже
	// держит `alpha#2` при занятой `alpha`, на повторное `alpha` получает то же `alpha#2`, и
	// переименовывать тему не во что.
	if fresh == oldName {
		return nil
	}

	err := c.withTopic(ctx, sess, workOther, func(topicID int64) error {
		return c.api.RenameTopic(ctx, topicID, fresh)
	})
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.topics.Rename(sess.disk.TopicID, fresh)
	sess.disk.TopicName = fresh

	c.mustSave(c.topics.Save(c.store))

	return nil
}

// freeName подбирает имя, которого в реестре нет: само название, иначе наименьший свободный
// номерной суффикс.
func (c *Core) freeName(name string, own int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	for num := 1; ; num++ {
		candidate := suffixed(name, num)
		if c.topics.FreeName(candidate, own) {
			return candidate
		}
	}
}

// bindTopic привязывает сессию к теме: прежняя свободная тема того же имени возвращается
// сессии, занятое живой привязкой имя получает наименьший свободный суффикс.
func (c *Core) bindTopic(ctx context.Context, sess *Session, name string) (bool, error) {
	candidate, entry, exists := c.pickTopic(name)

	if exists {
		c.mu.Lock()
		c.releaseTopic(entry.ID)
		sess.disk.TopicID = entry.ID
		sess.disk.TopicName = entry.Name
		c.mustSave(sess.disk.Save(c.store))
		c.mu.Unlock()

		return false, nil
	}

	topicID, err := c.api.CreateTopic(ctx, candidate)
	if err != nil {
		return false, errTopic(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.topics.Put(store.Topic{ID: topicID, Name: candidate, InMode: false, PinnedID: 0, IconUnknown: false})
	sess.disk.TopicID = topicID
	sess.disk.TopicName = candidate

	c.mustSave(c.topics.Save(c.store))
	c.mustSave(sess.disk.Save(c.store))

	return true, nil
}

// pickTopic выбирает имя темы: само название, иначе наименьший свободный номерной суффикс.
// Свободным считается имя, чьей темы в реестре нет или чья тема не занята живой привязкой.
func (c *Core) pickTopic(name string) (string, store.Topic, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for num := 1; ; num++ {
		candidate := suffixed(name, num)

		entry, ok := c.topics.ByNameLookup(candidate)
		if !ok {
			return candidate, store.Topic{ID: 0, Name: "", InMode: false, PinnedID: 0, IconUnknown: false}, false
		}

		if !c.topicOccupied(entry.ID) {
			return candidate, entry, true
		}
	}
}

// topicOccupied отвечает, занята ли тема живой привязкой. Занятая тема не забирается вовсе:
// вторая сессия получает свою, с суффиксом.
func (c *Core) topicOccupied(topicID int64) bool {
	sess := c.sessionByTopic(topicID)
	if sess == nil {
		return false
	}

	return sess.state(time.Now(), c.cfg, c.readyAt) != lifeDead
}

// releaseTopic снимает привязку прежней, уже не живой сессии: тема и сессия связаны один к
// одному, и двух привязок к одной теме не бывает.
func (c *Core) releaseTopic(topicID int64) {
	sess := c.sessionByTopic(topicID)
	if sess == nil {
		return
	}

	sess.disk.TopicID = 0
	sess.disk.TopicName = ""
	sess.disk.Mode = false
	c.mustSave(sess.disk.Save(c.store))
}

// withTopic делает работу с темой, переживая её удаление владельцем: ошибка «тема не найдена» —
// единственный способ узнать об удалении, поэтому тема заводится заново, в неё ставится строка
// состояния и значок, реестр чинится, и действие повторяется.
//
// Вид работы назван звонящим, потому что заведение темы заново само ставит ей значок и строку
// состояния: повторять то, что лечение уже сделало, не нужно — это был бы лишний вызов платформы.
func (c *Core) withTopic(
	ctx context.Context, sess *Session, work topicWork, action func(topicID int64) error,
) error {
	c.mu.Lock()

	topicID := sess.disk.TopicID
	name := sess.disk.TopicName

	c.mu.Unlock()

	if topicID == 0 {
		return protocol.Refuse(protocol.CodeNoTopic, "у сессии нет темы")
	}

	err := action(topicID)
	if err == nil {
		return nil
	}

	if !botapi.IsNoTopic(err) {
		return errTopic(err)
	}

	fresh, err := c.recreateTopic(ctx, sess, topicID, name)
	if err != nil {
		return err
	}

	if work != workOther {
		return nil
	}

	err = action(fresh)
	if err != nil {
		return errTopic(err)
	}

	return nil
}

// recreateTopic заводит пропавшую тему заново и чинит реестр.
func (c *Core) recreateTopic(ctx context.Context, sess *Session, oldID int64, name string) (int64, error) {
	c.journal(store.Entry{
		At: time.Now(), Event: "topic-gone", Session: sess.disk.ID, Outcome: "",
		Code: string(protocol.CodeNoTopic), Text: name, TopicID: oldID,
	})

	fresh, err := c.api.CreateTopic(ctx, name)
	if err != nil {
		return 0, errTopic(err)
	}

	c.mu.Lock()
	c.topics.Drop(oldID)
	c.topics.Put(store.Topic{ID: fresh, Name: name, InMode: false, PinnedID: 0, IconUnknown: false})
	sess.disk.TopicID = fresh
	inMode := sess.disk.Mode

	c.mustSave(c.topics.Save(c.store))
	c.mustSave(sess.disk.Save(c.store))
	c.mu.Unlock()

	if inMode {
		err = c.api.SetTopicInMode(ctx, fresh, true)
		if err != nil {
			return 0, errTopic(err)
		}

		c.mu.Lock()
		c.topics.Put(store.Topic{ID: fresh, Name: name, InMode: true, PinnedID: 0, IconUnknown: false})
		c.mustSave(c.topics.Save(c.store))
		c.mu.Unlock()
	}

	c.createPinned(ctx, sess, fresh, name)

	return fresh, nil
}

// setTopicMode переключает значок темы: грубое состояние несёт сам значок. Значок переключается
// только на изменение — лишний вызов платформы незачем делать на каждом включении режима.
// Состояние значка держит запись реестра, поэтому и перезапуск службы лишнего вызова не делает.
// Значок, поставленный мимо службы, запись делает неизвестным (`topicEdited`): тогда вызов уходит
// в любую сторону, а «уже так» клиент Bot API считает успехом.
func (c *Core) setTopicMode(ctx context.Context, sess *Session, inMode bool) {
	c.mu.Lock()
	entry, known := c.topics.ByIDLookup(sess.disk.TopicID)
	c.mu.Unlock()

	if known && !entry.IconUnknown && entry.InMode == inMode {
		return
	}

	err := c.withTopic(ctx, sess, workIcon, func(topicID int64) error {
		return c.api.SetTopicInMode(ctx, topicID, inMode)
	})
	if err != nil {
		c.journal(store.Entry{
			At: time.Now(), Event: "topic-icon-failed", Session: sess.disk.ID,
			Outcome: "", Code: string(botapi.Code(err)), Text: err.Error(), TopicID: 0,
		})

		// Исход отказа может быть неизвестен: сетевой обрыв после того, как Telegram значок уже
		// поставил. Запись становится «значок неизвестен», и следующее переключение уходит
		// платформе в любую сторону, а «уже так» клиент принимает успехом.
		c.markIconUnknown(sess.disk.TopicID)

		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, known = c.topics.ByIDLookup(sess.disk.TopicID)
	if !known {
		return
	}

	entry.InMode = inMode
	entry.IconUnknown = false
	c.topics.Put(entry)
	c.mustSave(c.topics.Save(c.store))
}

// markIconUnknown делает значок темы в реестре неизвестным; темы вне реестра не трогает.
func (c *Core) markIconUnknown(topicID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, known := c.topics.ByIDLookup(topicID)
	if !known || entry.IconUnknown {
		return
	}

	entry.IconUnknown = true
	c.topics.Put(entry)
	c.mustSave(c.topics.Save(c.store))
}

// ensurePinned ставит закреплённую строку, если её в теме нет. Создаётся она один раз и живёт,
// пока она в теме есть.
func (c *Core) ensurePinned(ctx context.Context, sess *Session, topicID int64, name string) {
	c.mu.Lock()
	entry, ok := c.topics.ByIDLookup(topicID)
	c.mu.Unlock()

	if ok && entry.PinnedID != 0 {
		return
	}

	c.createPinned(ctx, sess, topicID, name)
}

// createPinned отправляет строку состояния и закрепляет её: без закрепа её не видно в шапке
// темы. Служебный след от закрепа — цена восстановления.
func (c *Core) createPinned(ctx context.Context, sess *Session, topicID int64, name string) {
	c.mu.Lock()

	percent := sess.percent

	c.mu.Unlock()

	id, err := c.api.SendMessage(ctx, topicID, pinnedLine(name, percent))
	if err != nil {
		c.journal(store.Entry{
			At: time.Now(), Event: "pinned-failed", Session: sess.disk.ID,
			Outcome: "", Code: string(botapi.Code(err)), Text: err.Error(), TopicID: topicID,
		})

		return
	}

	err = c.api.PinMessage(ctx, topicID, id)
	if err != nil {
		c.journal(store.Entry{
			At: time.Now(), Event: "pin-failed", Session: sess.disk.ID, Outcome: "",
			Code: string(botapi.Code(err)), Text: err.Error(), TopicID: topicID,
		})

		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.topics.ByIDLookup(topicID)
	if !ok {
		return
	}

	entry.PinnedID = id
	c.topics.Put(entry)
	c.mustSave(c.topics.Save(c.store))
}

// editPinned правит закреплённую строку на месте. Правка текста служебных записей в теме не
// создаёт; пропала сама строка — она отправляется и закрепляется снова.
func (c *Core) editPinned(ctx context.Context, sess *Session, topicID int64, name string, percent int) {
	c.mu.Lock()
	entry, ok := c.topics.ByIDLookup(topicID)
	c.mu.Unlock()

	if !ok || entry.PinnedID == 0 {
		c.createPinned(ctx, sess, topicID, name)

		return
	}

	err := c.api.EditMessageText(ctx, entry.PinnedID, pinnedLine(name, percent))
	if err == nil {
		return
	}

	c.journal(store.Entry{
		At: time.Now(), Event: "pinned-edit-failed", Session: sess.disk.ID,
		Outcome: "", Code: string(botapi.Code(err)), Text: err.Error(), TopicID: topicID,
	})

	c.mu.Lock()

	entry.PinnedID = 0
	c.topics.Put(entry)
	c.mustSave(c.topics.Save(c.store))
	c.mu.Unlock()

	c.createPinned(ctx, sess, topicID, name)
}

// sendChunks отправляет текст в тему, нарезая его по пределу сообщения, кусками, по порядку.
// Режет и мерит `botapi`: предел и счёт единиц UTF-16 знает слой платформы, и мерило у нарезки,
// правки закреплённой строки и отправки одно.
func (c *Core) sendChunks(ctx context.Context, sess *Session, text string) ([]int, error) {
	chunks := botapi.Cut(text)
	ids := make([]int, 0, len(chunks))

	for _, part := range chunks {
		var id int

		err := c.withTopic(ctx, sess, workOther, func(dest int64) error {
			sent, err := c.api.SendMessage(ctx, dest, part)
			if err != nil {
				return fmt.Errorf("кусок текста не отправлен: %w", err)
			}

			id = sent

			return nil
		})
		if err != nil {
			return ids, err
		}

		ids = append(ids, id)
	}

	return ids, nil
}

// pickName берёт имя темы из аргумента, иначе из привязки сессии, иначе из настройки. Имя не
// выводится из имени каталога дерева и не спрашивается у системы контроля версий.
func pickName(requested, bound, fallback string) string {
	if requested != "" {
		return requested
	}

	if bound != "" {
		return bound
	}

	return fallback
}

// suffixed собирает имя темы с номером: первый номер — само название без суффикса. Номер встаёт в
// пределы платформы вместе с именем — режется начало, а не номер: иначе вторая тема длинного имени
// звалась бы в Telegram так же, как первая.
func suffixed(name string, num int) string {
	if num <= 1 {
		return name
	}

	return botapi.TopicName(name, fmt.Sprintf("#%d", num))
}

// pinnedLine собирает закреплённую строку по шаблону владельца: процент и название. Имя темы
// резано по пределу платформы при входе в реестр, поэтому строка из него в предел сообщения
// укладывается всегда.
func pinnedLine(name string, percent int) string {
	if percent <= 0 {
		return name
	}

	return fmt.Sprintf("%d%% %s", percent, name)
}
