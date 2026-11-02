package core

import (
	"context"
	"fmt"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// Handle исполняет один запрос соединения. Ответ сопоставляется с запросом по `rid`, поэтому
// единственный долгий запрос — `delivery.wait` — не мешает коротким идти мимо него.
//
// Исход каждого запроса проходит через вклад в статусную строку: отказ самого моста владелец
// обязан видеть в полосе, а не узнавать по тишине в теме.
func (c *Core) Handle(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	resp := c.answer(ctx, conn, req)

	c.noteFault(conn, req, resp)

	return resp
}

// answer исполняет запрос: версия, приветствие, граница роли и разбор операции.
func (c *Core) answer(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	if req.V != protocol.Version {
		c.journal(store.Entry{
			At: time.Now(), Event: "version-mismatch", Session: req.Session, Outcome: "",
			Code: string(protocol.CodeVersion), Text: string(req.Op), TopicID: 0,
		})

		return protocol.Fail(req.RID, protocol.CodeVersion,
			fmt.Sprintf("служба v%d, звонящий v%d", protocol.Version, req.V))
	}

	role, greeted := c.connInfo(conn)

	// Второй `hello` на том же соединении — негодный вызов, а не повторное приветствие: роль и
	// сессию соединение называет один раз, иначе одно соединение успело бы побыть двумя.
	if req.Op == protocol.OpHello {
		if greeted {
			return protocol.Fail(req.RID, protocol.CodeBadRequest, "`hello` второй раз на том же соединении")
		}

		return c.hello(conn, req)
	}

	if !greeted {
		return protocol.Fail(req.RID, protocol.CodeBadRequest, "первый кадр соединения — `hello`")
	}

	if !allowed(role, req.Op) {
		return protocol.Fail(req.RID, protocol.CodeBadRequest,
			"операция `"+string(req.Op)+"` этому звонящему недоступна")
	}

	return c.dispatch(ctx, conn, req)
}

// dispatch разводит операцию по обработчику. Операции, которой в этой версии нет, отвечает
// `unknown-op`: молчаливого разбора «по тем полям, что понял» у протокола нет.
//
//nolint:cyclop // полный switch по операциям контракта, а не сложность: ветка на операцию
func (c *Core) dispatch(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	switch req.Op {
	case protocol.OpModeOn:
		return c.modeOn(ctx, conn, req)
	case protocol.OpModeOff:
		return c.modeOff(ctx, conn, req)
	case protocol.OpModeStatus:
		return c.modeStatus(conn, req)
	case protocol.OpServiceStatus:
		return c.serviceStatus(req)
	case protocol.OpTurnStart:
		return c.turnStart(ctx, conn, req)
	case protocol.OpTurnEnd:
		return c.turnEnd(ctx, conn, req)
	case protocol.OpQuestionAsk:
		return c.questionAsk(ctx, conn, req)
	case protocol.OpDeliveryWait:
		return c.deliveryWait(ctx, conn, req)
	case protocol.OpDeliveryAck:
		return c.deliveryAck(ctx, conn, req)
	case protocol.OpSay:
		return c.say(ctx, conn, req)
	case protocol.OpAgentMessage:
		return c.agentMessage(ctx, conn, req)
	case protocol.OpContextReport:
		return c.contextReport(ctx, conn, req)
	case protocol.OpNameReport:
		return c.nameReport(ctx, conn, req)
	case protocol.OpBye:
		return c.bye(conn, req)
	case protocol.OpHello:
		panic("`hello` до разбора операций: его перехватывает Handle")
	}

	return protocol.Fail(req.RID, protocol.CodeUnknownOp, "операции `"+string(req.Op)+"` в этой версии нет")
}

// modeStatus отдаёт состояние режима тонкому клиенту и чужому тулингу.
func (c *Core) modeStatus(conn *Conn, req protocol.Request) protocol.Response {
	c.mu.Lock()
	defer c.mu.Unlock()

	resp := protocol.Ok(req.RID)
	sess := c.session(sessionOf(conn, req))

	if sess == nil {
		resp.Mode = protocol.ModeOff

		return resp
	}

	resp.Mode = modeOf(sess)
	resp.TopicID = sess.disk.TopicID
	resp.TopicName = sess.disk.TopicName

	return resp
}

// bye — средство сессии кончило сессию: служба хоронит её сама и без владельца. Сети похороны
// не касаются — режим остаётся, значок темы не меняется, — поэтому ctx операции тут не нужен, как
// и в `mode.status`.
//
// Хоронит сессию то же соединение, что её держит. Вытесненное прежнее — мод перезагрузился, и
// погашенная подкоманда говорит `bye` уже после `hello` новой — иначе похоронило бы сессию
// вместе с новым соединением и отпустило бы его долгий запрос: канала не стало бы до перезапуска
// клиента. Дальше вытесненное соединение о сессии ничего не решает, как и в прочих кадрах.
// Похороненной сессии повторный `bye` отвечает `ok`: похороны идемпотентны, а мод и его подкоманда
// на выходе могут сказать `bye` оба.
func (c *Core) bye(conn *Conn, req protocol.Request) protocol.Response {
	c.mu.Lock()
	sess := c.session(sessionOf(conn, req))

	if sess != nil && !sess.disk.Buried && !c.holdsSession(sess, conn) {
		c.mu.Unlock()

		return protocol.Fail(req.RID, protocol.CodeBadRequest, "`bye` зовёт не соединение сессии")
	}

	c.mu.Unlock()

	if sess == nil {
		return protocol.Ok(req.RID)
	}

	c.bury(sess, "сессия кончилась")

	return protocol.Ok(req.RID)
}

// connInfo читает роль соединения под мьютексом: запросы одного соединения идут вразнобой. Вид
// звонящего в разрешениях не участвует — клиенты равны, и мод с адаптером различать незачем.
func (c *Core) connInfo(conn *Conn) (protocol.Role, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return conn.role, conn.greeted
}

// sessionInMode находит сессию звонящего и проверяет, что режим включён.
func (c *Core) sessionInMode(conn *Conn, req protocol.Request) (*Session, *protocol.Response) {
	c.mu.Lock()
	defer c.mu.Unlock()

	sess := c.session(sessionOf(conn, req))
	if sess == nil {
		fail := protocol.Fail(req.RID, protocol.CodeNoMode, "сессия службе неизвестна")

		return nil, &fail
	}

	if !sess.disk.Mode {
		fail := protocol.Fail(req.RID, protocol.CodeNoMode, "у сессии не включён режим")

		return nil, &fail
	}

	return sess, nil
}

// allowed держит границу роли, и только роли: вид звонящего здесь не участвует — мод Claude Code
// и плагин OpenCode равны. Поток сообщений агента, границы хода и имя чата ведёт соединение
// сессии; короткому вызову доступны режим, «сказать владельцу» и вопрос с вариантами — своего
// вывода под видом реплики агента в тему не сольёт никто, и чужое имя чата короткий вызов тоже не
// назовёт.
func allowed(role protocol.Role, op protocol.Op) bool {
	if role == protocol.RoleSession {
		return true
	}

	switch op {
	case protocol.OpModeOn, protocol.OpModeOff, protocol.OpModeStatus, protocol.OpServiceStatus,
		protocol.OpSay, protocol.OpQuestionAsk:
		return true
	case protocol.OpHello, protocol.OpTurnStart, protocol.OpTurnEnd, protocol.OpDeliveryWait,
		protocol.OpDeliveryAck, protocol.OpAgentMessage, protocol.OpContextReport,
		protocol.OpNameReport, protocol.OpBye:
		return false
	}

	return false
}

// sessionOf отдаёт id сессии запроса. Роль `session` распоряжается только своей сессией — той,
// которую назвала своим `hello`. Поле кадра читается у роли `tool`: тонкий клиент так называет
// сессию, о которой говорит.
func sessionOf(conn *Conn, req protocol.Request) string {
	if conn.role == protocol.RoleTool && req.Session != "" {
		return req.Session
	}

	return conn.session
}
