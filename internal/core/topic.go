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

	// Режим в закрытой теме не живёт: команда `/away` у кончившейся сессии сперва возвращает тему.
	c.restoreTopic(ctx, sess)

	c.mu.Lock()

	// Имя режется по пределу платформы здесь, на входе в реестр: реестр держит ровно то имя, что
	// держит Telegram, и закреплённая строка, собранная из него, за предел сообщения не уходит.
	name := botapi.TopicName(pickName(requested, sess.disk.AutoName, sess.disk.TopicName,
		c.cfg.DefaultTopicName, sess.disk.NameManual), "")
	bound := sess.disk.TopicID != 0
	oldName := sess.disk.TopicName
	// Имя, названное владельцем аргументом, старше имени чата и остаётся таким до следующего
	// слова владельца. Включение без аргумента ручного имени не отменяет.
	manual := sess.disk.NameManual || requested != ""

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
	sess.disk.ModeOnReturn = false
	sess.disk.NameManual = manual
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

// nameReport принимает имя, названное средством сессии: название чата. Служба не знает, что это
// чат, — ему передают обычное имя, — и решает по признаку у себя: имя, заданное владельцем
// вручную, старше, а без ручного имени тема следует за названным.
func (c *Core) nameReport(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	c.mu.Lock()
	sess := c.session(sessionOf(conn, req))
	c.mu.Unlock()

	if sess == nil {
		return protocol.Fail(req.RID, protocol.CodeBadRequest, "`name.report` без сессии")
	}

	err := c.followName(ctx, sess, req.Name)
	if err != nil {
		return failure(req.RID, err)
	}

	return protocol.Ok(req.RID)
}

// followName запоминает название чата и переименовывает привязанную тему, если имя не задано
// вручную. Переименование идёт тем же путём, что и слово владельца: занятое имя получает номерной
// суффикс, то же имя не переименовывается, а пропавшая тема заводится заново.
//
// Имя снимается пустым: у чата названия нет, и тема остаётся при прежнем имени — так же, как
// `mode.on` без аргумента берёт привязку сессии.
func (c *Core) followName(ctx context.Context, sess *Session, name string) error {
	c.topicMu.Lock()
	defer c.topicMu.Unlock()

	c.mu.Lock()

	sess.disk.AutoName = name
	manual := sess.disk.NameManual
	// Закрытую тему название чата не правит: оно запомнено, и с ним сессия вернётся.
	bound := sess.disk.TopicID != 0 && !c.topics.IsClosed(sess.disk.TopicID)
	oldName := sess.disk.TopicName

	c.mustSave(sess.disk.Save(c.store))
	c.mu.Unlock()

	if manual || !bound || name == "" {
		return nil
	}

	// Имя режется по пределу платформы здесь же, как при включении режима: реестр держит ровно то
	// имя, что держит Telegram, иначе переименование записало бы в реестр имя, которого у темы нет.
	err := c.renameBound(ctx, sess, oldName, botapi.TopicName(name, ""))
	if err != nil {
		return err
	}

	c.mu.Lock()
	renamed := sess.disk.TopicName != oldName
	c.mu.Unlock()

	if renamed {
		// Имя темы уезжает в статусную строку: своего состояния она не читает, и прежнее имя
		// осталось бы на экране до следующего вклада.
		c.contribute(sess)
	}

	return nil
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
	sess.disk.ModeOnReturn = false
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

	topicID := sess.disk.TopicID
	percent := sess.percent

	c.topics.Rename(topicID, fresh)
	sess.disk.TopicName = fresh

	c.mustSave(c.topics.Save(c.store))
	c.mu.Unlock()

	// Закреплённая строка носит то же имя, что и тема: без этой правки в шапке осталось бы прежнее,
	// и владелец читал бы в одной теме два разных имени — а у клиента, который процента контекста
	// не присылает, прежнее имя осталось бы там навсегда.
	c.editPinned(ctx, sess, topicID, fresh, percent)

	return nil
}

// freeName подбирает имя, которого в реестре нет: само название, иначе наименьший свободный
// номерной суффикс.
func (c *Core) freeName(name string, own int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.leastFreeName(name, own)
}

// leastFreeName — подбор свободного имени; звать под `mu`.
func (c *Core) leastFreeName(name string, own int64) string {
	for num := 1; ; num++ {
		candidate := suffixed(name, num)
		if c.topics.FreeName(candidate, own) {
			return candidate
		}
	}
}

// restoreTopic открывает закрытую тему сессии и ставит значок по режиму. Имя при закрытии не
// менялось и правится, только если занято: тогда наименьший свободный суффикс. Темы, которую служба
// не закрывала, не касается. Отказ платформы ложится в журнал и возврата не отменяет. Звать под
// `topicMu`.
func (c *Core) restoreTopic(ctx context.Context, sess *Session) {
	c.mu.Lock()

	topicID := sess.disk.TopicID

	if !c.topics.IsClosed(topicID) {
		c.mu.Unlock()

		return
	}

	entry, _ := c.topics.ByIDLookup(topicID)
	name := entry.Name
	if !c.topics.FreeName(name, topicID) {
		base, _ := splitTopicNumber(name)
		name = c.leastFreeName(base, topicID)
	}

	renamed := name != entry.Name
	inMode := sess.disk.Mode

	c.topics.Reopen(topicID, name)
	sess.disk.TopicName = name
	c.mustSave(c.topics.Save(c.store))
	c.mustSave(sess.disk.Save(c.store))
	c.mu.Unlock()

	err := c.api.ReopenTopic(ctx, topicID)
	if botapi.IsNoTopic(err) {
		_, err = c.recreateTopic(ctx, sess, topicID, name)
		if err != nil {
			c.journalTopicRefusal(sess, "topic-reopen-failed", topicID, err)
		}

		return
	}

	if err != nil {
		c.journalTopicRefusal(sess, "topic-reopen-failed", topicID, err)
	}

	if renamed {
		err = c.api.RenameTopic(ctx, topicID, name)
		if err != nil {
			c.journalTopicRefusal(sess, "topic-rename-failed", topicID, err)
		}
	}

	c.setTopicMode(ctx, sess, inMode)
}

// journalTopicRefusal оставляет отказ платформы по теме в журнале.
func (c *Core) journalTopicRefusal(sess *Session, event string, topicID int64, err error) {
	c.journal(store.Entry{
		At: time.Now(), Event: event, Session: sess.disk.ID, Outcome: "",
		Code: string(botapi.Code(err)), Text: err.Error(), TopicID: topicID,
	})
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
	sess.disk.ModeOnReturn = false
	// Ручное имя жило в имени темы: темы у сессии больше нет, и имени, которое надо было бы
	// помнить, тоже. Название чата — факт о сессии, а не о теме, и остаётся.
	sess.disk.NameManual = false
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

	// Закрытая тема имени не держит: пока её не было в реестре имён, его могла занять другая.
	name = c.freeName(name, oldID)

	fresh, err := c.api.CreateTopic(ctx, name)
	if err != nil {
		return 0, errTopic(err)
	}

	c.mu.Lock()
	c.topics.Drop(oldID)
	// У новой темы значок платформы по умолчанию, ни вкл, ни выкл: пока он не поставлен, он неизвестен.
	c.topics.Put(store.Topic{ID: fresh, Name: name, InMode: false, PinnedID: 0, IconUnknown: true})
	sess.disk.TopicID = fresh
	sess.disk.TopicName = name
	inMode := sess.disk.Mode

	c.mustSave(c.topics.Save(c.store))
	c.mustSave(sess.disk.Save(c.store))
	c.mu.Unlock()

	err = c.api.SetTopicInMode(ctx, fresh, inMode)
	if err != nil {
		return 0, errTopic(err)
	}

	c.noteIcon(fresh, inMode)
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

	c.noteIcon(sess.disk.TopicID, inMode)
}

// noteIcon записывает в реестр значок, поставленный на платформе; темы вне реестра не трогает.
func (c *Core) noteIcon(topicID int64, inMode bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, known := c.topics.ByIDLookup(topicID)
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

// pickName берёт имя темы из аргумента, иначе из названия чата, иначе из привязки сессии, иначе из
// настройки. Имя не выводится из имени каталога дерева и не спрашивается у системы контроля
// версий. Название чата старше привязки, но младше слова владельца: заданное вручную имя
// переименования чата не меняют.
func pickName(requested, auto, bound, fallback string, manual bool) string {
	if requested != "" {
		return requested
	}

	if !manual && auto != "" {
		return auto
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

// pinnedLine собирает закреплённую строку: процент. Имя темы и так видно в её шапке, поэтому оно
// идёт в строку только пока процента нет: сообщение не может быть пустым. Имя темы резано по
// пределу платформы при входе в реестр, поэтому в предел сообщения укладывается всегда.
func pinnedLine(name string, percent int) string {
	if percent <= 0 {
		return name
	}

	return fmt.Sprintf("%d%%", percent)
}
