package core

import (
	"context"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// Conn — соединение со службой. Соединений два вида, и они не равны по смыслу: короткий вызов
// живости не несёт, соединение сессии её и держит.
type Conn struct {
	id      uint64
	role    protocol.Role
	kind    protocol.Kind
	session string
	greeted bool
	closed  bool
}

// Session отдаёт id сессии, назвавшейся на этом соединении.
func (n *Conn) Session() string {
	return n.session
}

// NewConn заводит соединение: до `hello` у него нет ни роли, ни сессии.
func (c *Core) NewConn() *Conn {
	c.mu.Lock()
	defer c.mu.Unlock()

	conn := &Conn{
		id:      c.nextConn,
		role:    "",
		kind:    "",
		session: "",
		greeted: false,
		closed:  false,
	}
	c.nextConn++

	return conn
}

// CloseConn закрывает соединение. Своё соединение сессия теряет только тогда, когда закрылось
// именно оно: вытесненное прежнее соединение её живости уже не держит и своим закрытием ничего
// не меняет.
func (c *Core) CloseConn(ctx context.Context, conn *Conn) {
	c.mu.Lock()

	if conn.closed {
		c.mu.Unlock()

		return
	}

	conn.closed = true

	sess := c.session(conn.session)
	if sess == nil || sess.conn != conn {
		c.mu.Unlock()

		return
	}

	sess.conn = nil
	sess.lostAt = time.Now()
	// Ход кончился обрывом: держать «печатает» за него больше некому, а имя хода живёт у
	// средства сессии — новое соединение начинает счёт имён заново.
	sess.forgetTurn()

	undelivered := make([]*store.Record, len(sess.inbox))
	copy(undelivered, sess.inbox)

	c.mu.Unlock()

	// Слово владельца, которое сессия не успела подтвердить, остаётся в ящике и уедет
	// вернувшемуся моду — а владелец читает это на своём же сообщении: у агента слова нет.
	c.hold(ctx, sess, undelivered...)
}

// hello — первый кадр соединения: версия, роль, вид звонящего, сессия. Он же забирает соединение
// сессии себе.
func (c *Core) hello(conn *Conn, req protocol.Request) protocol.Response {
	kind := protocol.Kind(req.Kind)

	err := validateHello(req, kind)
	if err != nil {
		return failure(req.RID, err)
	}

	c.mu.Lock()
	c.greet(conn, req, kind)

	if req.Role == protocol.RoleTool {
		c.mu.Unlock()

		return c.toolGreeting(req)
	}

	sess := c.sessionOrNew(req)
	sess.adopt(req)

	displaced := c.takeOver(sess, conn)
	resp := c.greeting(sess, req)
	c.mustSave(sess.disk.Save(c.store))
	c.mu.Unlock()

	// Вытесненное соединение узнаёт об этом концом своего долгого запроса: висеть ему больше не
	// на чем, а сессия теперь не его.
	for _, waiter := range displaced {
		waiter.resolve(nil)
	}

	if resp.Revived {
		c.logRevival(sess)
	}

	return resp
}

// greet отмечает соединение представившимся: с этого момента ему доступны операции его роли.
func (c *Core) greet(conn *Conn, req protocol.Request, kind protocol.Kind) {
	conn.greeted = true
	conn.role = req.Role
	conn.kind = kind
	conn.session = req.Session
}

// takeOver забирает соединение сессии новому соединению и отдаёт ждущих прежнего. Новое
// соединение вытесняет прежнее, потому что настоящий случай здесь один и гоночный: мод
// перезапустился, а прежнее соединение ещё не похоронено отсрочкой.
func (c *Core) takeOver(sess *Session, conn *Conn) []*deliveryWaiter {
	previous := sess.conn
	sess.conn = conn
	sess.lostAt = time.Time{}

	if previous == nil || previous == conn {
		return nil
	}

	previous.closed = true

	// Прежнее соединение сессию потеряло: его ход кончился вместе с ним, а имя хода, пережившее
	// перехват, съело бы первый ход нового соединения — счёт имён у него начинается заново.
	sess.forgetTurn()

	displaced := sess.deliveryWaiters
	sess.deliveryWaiters = nil

	c.journal(store.Entry{
		At: time.Now(), Event: "session-conn-displaced", Session: sess.disk.ID,
		Outcome: "", Code: "", Text: "", TopicID: sess.disk.TopicID,
	})

	return displaced
}

// greeting собирает ответ на `hello` и снимает отметку похорон: вернувшаяся сессия получает и
// свою тему, и режим таким, каким он был. Режим снимает только слово владельца, поэтому здесь не
// снимается ничего — средство сессии узнаёт о самом возврате полем `revived`.
func (c *Core) greeting(sess *Session, req protocol.Request) protocol.Response {
	revived := sess.disk.Buried
	if revived {
		sess.disk.Buried = false
	}

	resp := protocol.Ok(req.RID)
	resp.Mode = modeOf(sess)
	resp.TopicID = sess.disk.TopicID
	resp.TopicName = sess.disk.TopicName
	resp.Revived = revived

	return resp
}

// toolGreeting — ответ короткому вызову: тонкому клиенту и чужому тулингу. Сессию он называет,
// если говорит о ней, а живости не несёт.
func (c *Core) toolGreeting(req protocol.Request) protocol.Response {
	resp := protocol.Ok(req.RID)
	resp.Mode = protocol.ModeOff

	sess := c.session(req.Session)
	if sess == nil {
		return resp
	}

	resp.Mode = modeOf(sess)
	resp.TopicID = sess.disk.TopicID
	resp.TopicName = sess.disk.TopicName

	return resp
}

// logRevival оставляет возврат похороненной сессии в журнале. В тему о нём не пишется ничего и в
// сессию ничего не подаётся: служебных строк о жизни сессии не бывает, а о том, что режима нет,
// средство сессии узнаёт полем `revived` в ответе на своё же `hello`.
func (c *Core) logRevival(sess *Session) {
	c.journal(store.Entry{
		At: time.Now(), Event: "session-revived", Session: sess.disk.ID,
		Outcome: "", Code: "", Text: "", TopicID: sess.disk.TopicID,
	})
}

// validateHello проверяет обязательные поля первого кадра. Роль сессии занимает только мод или
// адаптер: сессии без мода не бывает, и честный ответ на такое приветствие — отказ с названной
// причиной, а не полуживая сессия, которой нечем подать слово владельца.
func validateHello(req protocol.Request, kind protocol.Kind) error {
	if req.V != protocol.Version {
		return protocol.Refuse(protocol.CodeVersion, "служба v%d, звонящий v%d", protocol.Version, req.V)
	}

	switch req.Role {
	case protocol.RoleSession, protocol.RoleTool:
	default:
		return protocol.Refuse(protocol.CodeBadRequest, "негодная роль %q", req.Role)
	}

	switch kind {
	case protocol.KindMod, protocol.KindAdapter, protocol.KindTool:
	default:
		return protocol.Refuse(protocol.CodeBadRequest, "негодный вид звонящего %q", req.Kind)
	}

	if req.Role != protocol.RoleSession {
		return nil
	}

	if req.Session == "" {
		return protocol.Refuse(protocol.CodeBadRequest, "роль session без `session`")
	}

	if kind != protocol.KindMod && kind != protocol.KindAdapter {
		return protocol.Refuse(protocol.CodeBadRequest,
			"сессию держит только мод или адаптер: сессии без мода не бывает, вид звонящего %q", kind)
	}

	return nil
}

// modeOf переводит режим привязки в значение протокола.
func modeOf(sess *Session) protocol.Mode {
	if sess.disk.Mode {
		return protocol.ModeOn
	}

	return protocol.ModeOff
}
