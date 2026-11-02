package core

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"

	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// lifeState — живость сессии. Номер процесса её не решает нигде: живость держит открытое
// соединение сессии, а смерть измеряется сроком.
type lifeState int

const (
	// lifeAlive — соединение сессии открыто.
	lifeAlive lifeState = iota
	// lifeDoubtful — соединение оборвано меньше `reconnect_grace` назад.
	lifeDoubtful
	// lifeDead — `bye` либо истёкшая отсрочка возврата.
	lifeDead
)

// Session — сессия в процессе службы: привязка с диска плюс всё, что живёт только в памяти.
type Session struct {
	disk store.Session

	// conn — соединение сессии. Оно одно: новое соединение той же сессии вытесняет прежнее,
	// потому что настоящий случай здесь один — мод перезапустился. Оно же и есть живость.
	conn   *Conn
	lostAt time.Time

	deliveryWaiters []*deliveryWaiter
	inbox           []*store.Record

	// handed помнит, какому соединению запись уже отдавалась: id соединения по id записи.
	// Отдача приёмом не считается, поэтому запись остаётся в ящике — и без этой памяти та же
	// запись ушла бы тому же моду в цикле, круг за кругом. Новое соединение сессии получает
	// новый id, значит придержанное слово подаётся заново само собой, как только сессия
	// подключилась.
	handed map[string]uint64

	lastEdit time.Time
	percent  int

	// fault — последний отказ самого моста по этой сессии: он уезжает полем `error` вклада в
	// статусную строку. На диске его нет: причина отказа канала верна ровно пока жив процесс,
	// который её узнал, а перезапуск службы её не наследует.
	fault string
	// contributedAt — когда вклад в статусную строку писался последний раз. По нему сторож
	// освежает вклад тактом службы, чтобы срок годности не переживал интервал между записями.
	contributedAt time.Time

	typingStop context.CancelFunc

	// turn — ход, чьё действие чата держится, и turnDone — ход, чей конец уже пришёл. Границы
	// хода идут извещениями, которые ответа не ждут, поэтому порядок их прихода не обещан ничем:
	// по этим двум ход гасит только своё «печатает», а начало, опоздавшее за собственным концом,
	// хода не начинает вовсе.
	turn     string
	turnDone string
}

// state отдаёт живость сессии. Отсрочка возврата считается от позднейшего из двух моментов —
// обрыва и старта службы: иначе переустановка службы хоронила бы разом все живые сессии.
func (s *Session) state(now time.Time, cfg *config.Config, readyAt time.Time) lifeState {
	// Похороненная сессия мертва, пока не вернулась своим `hello`: режим она не держит, и тема её
	// больше не занята.
	if s.disk.Buried {
		return lifeDead
	}

	if s.alive() {
		return lifeAlive
	}

	from := s.lostAt
	if from.Before(readyAt) {
		from = readyAt
	}

	if now.Sub(from) < cfg.ReconnectGrace {
		return lifeDoubtful
	}

	return lifeDead
}

// alive отвечает, открыто ли соединение сессии.
func (s *Session) alive() bool {
	return s.conn != nil && !s.conn.closed
}

// adopt перенимает из кадра то, что сессия о себе называет. Пустое поле прежнего значения не
// стирает — его мог назвать другой звонящий той же сессии.
func (s *Session) adopt(req protocol.Request) {
	if req.Client != "" {
		s.disk.Client = req.Client
	}
}

// pendingDelivery отдаёт старшую запись, которую этому соединению ещё не подавали. `after` —
// память самого мода о последней принятой им доставке; служба знает то же своей памятью, так что
// пустой `after` ничего не ломает.
func (s *Session) pendingDelivery(after string, connID uint64) *store.Record {
	for _, rec := range s.inbox {
		if rec.ID != after && s.handed[rec.ID] != connID {
			return rec
		}
	}

	return nil
}

// markHanded помнит, что запись подана этому соединению. Второй отметки это не ставит: приём
// подтверждает мод, а не служба.
func (s *Session) markHanded(rec *store.Record, connID uint64) {
	s.handed[rec.ID] = connID
}

// record отдаёт запись ящика по её id.
func (s *Session) record(id string) *store.Record {
	for _, rec := range s.inbox {
		if rec.ID == id {
			return rec
		}
	}

	return nil
}

// dropRecord убирает запись из ящика в памяти вместе с памятью о её подаче.
func (s *Session) dropRecord(id string) {
	delete(s.handed, id)

	kept := make([]*store.Record, 0, len(s.inbox))

	for _, rec := range s.inbox {
		if rec.ID != id {
			kept = append(kept, rec)
		}
	}

	s.inbox = kept
}

// stopTyping гасит действие чата «печатает», если оно держится.
func (s *Session) stopTyping() {
	if s.typingStop == nil {
		return
	}

	s.typingStop()
	s.typingStop = nil
}

// forgetTurn гасит «печатает» и забывает имена ходов: соединения у сессии больше нет, а имя хода
// живёт у средства сессии, а не у службы. У OpenCode имя — счёт ходов соединения, и новое
// соединение начинает его заново; память службы, пережившая обрыв, молча съела бы первый ход после
// восстановления — его начало совпало бы по имени с концом, пришедшим до обрыва.
func (s *Session) forgetTurn() {
	s.stopTyping()
	s.turn = ""
	s.turnDone = ""
}

// buriedTopicPrefix — приставка имени темы кончившейся сессии.
const buriedTopicPrefix = "[кончилась] "

// minTopicNumber — первый номер в номерном суффиксе: первая тема имени зовётся без номера.
const minTopicNumber = 2

// bury хоронит сессию: её тема закрывается и освобождает имя, ждущие отпускаются, соединения у неё
// больше нет. Режим погребённая не держит, но и не теряет: насовсем его снимает только слово
// владельца, поэтому вернувшееся средство сессии застаёт режим таким, каким он был. Значок темы не
// меняется. Владельцу о похоронах не пишется: служебных строк о жизни сессии в теме нет вовсе,
// состояние он видит видом самой темы и вкладом в статусную строку.
func (c *Core) bury(ctx context.Context, sess *Session, reason string) {
	c.mu.Lock()

	if sess.disk.Buried {
		c.mu.Unlock()

		return
	}

	sess.disk.Buried = true
	sess.conn = nil
	waiters := sess.deliveryWaiters
	sess.deliveryWaiters = nil
	topicID := sess.disk.TopicID
	sess.forgetTurn()

	if sess.disk.Mode {
		sess.disk.Mode = false
		sess.disk.ModeOnReturn = true
	}

	c.mustSave(sess.disk.Save(c.store))
	c.mu.Unlock()

	for _, waiter := range waiters {
		waiter.resolve(nil)
	}

	c.journal(store.Entry{
		At: time.Now(), Event: "session-buried", Session: sess.disk.ID,
		Outcome: reason, Code: "", Text: "", TopicID: topicID,
	})

	c.contribute(sess)
	c.closeBuriedTopic(context.WithoutCancel(ctx), sess)
}

// closeBuriedTopic закрывает тему погребённой сессии и правит её имя. Реестр освобождает имя сразу:
// отказ платформы погребения не откатывает, а только ложится в журнал. Заводить заново здесь нечего:
// тема, удалённая руками, кончившейся сессии не нужна, а вернувшейся её заведёт `restoreTopic`.
//
// Отмена запроса работу не обрывает: `bye` приходит на выходе, и соединение может закрыться раньше,
// чем платформа ответит.
func (c *Core) closeBuriedTopic(ctx context.Context, sess *Session) {
	c.topicMu.Lock()
	defer c.topicMu.Unlock()

	c.mu.Lock()

	topicID := sess.disk.TopicID
	entry, known := c.topics.ByIDLookup(topicID)

	// Сессия могла вернуться, пока эта работа ждала своей очереди: тогда закрывать нечего.
	if !sess.disk.Buried || !known || c.topics.IsClosed(topicID) {
		c.mu.Unlock()

		return
	}

	c.topics.Close(topicID)
	c.mustSave(c.topics.Save(c.store))
	c.mu.Unlock()

	err := c.api.RenameTopic(ctx, topicID, buriedTopicName(entry.Name))
	if botapi.IsNoTopic(err) {
		c.journalTopicRefusal(sess, "topic-gone", topicID, err)

		return
	}

	if err != nil {
		c.journalTopicRefusal(sess, "topic-rename-failed", topicID, err)
	}

	err = c.api.CloseTopic(ctx, topicID)
	if err != nil {
		c.journalTopicRefusal(sess, "topic-close-failed", topicID, err)
	}
}

// buriedTopicName собирает имя темы кончившейся сессии. Приставка входит в предел платформы, а номер
// остаётся на конце: режется начало, как у всякого имени с номером.
func buriedTopicName(name string) string {
	base, number := splitTopicNumber(name)

	return botapi.TopicName(buriedTopicPrefix+base, number)
}

// splitTopicNumber отделяет от имени темы номерной суффикс; суффикса нет — хвост пуст.
func splitTopicNumber(name string) (string, string) {
	at := strings.LastIndex(name, "#")
	if at <= 0 {
		return name, ""
	}

	num, err := strconv.Atoi(name[at+1:])
	if err != nil || num < minTopicNumber {
		return name, ""
	}

	return name[:at], name[at:]
}

// newSession заводит сессию в памяти на её привязке с диска.
func newSession(disk store.Session) *Session {
	return &Session{
		disk:            disk,
		conn:            nil,
		lostAt:          time.Time{},
		deliveryWaiters: nil,
		inbox:           nil,
		handed:          map[string]uint64{},
		lastEdit:        time.Time{},
		percent:         0,
		fault:           "",
		contributedAt:   time.Time{},
		typingStop:      nil,
		turn:            "",
		turnDone:        "",
	}
}
