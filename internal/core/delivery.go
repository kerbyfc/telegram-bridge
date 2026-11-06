package core

import (
	"context"
	"sync"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// deliveryWaiter — долгий запрос `delivery.wait` соединения сессии. Это единственное долгое
// ожидание протокола: ожидания хода у службы нет.
type deliveryWaiter struct {
	connID uint64
	after  string
	ch     chan *store.Record
	once   sync.Once
}

// resolve отдаёт доставку ждущему; nil значит «соединение больше не сессии». Канал буферизован,
// поэтому служба не висит на ждущем, который уже ушёл.
func (w *deliveryWaiter) resolve(rec *store.Record) {
	w.once.Do(func() { w.ch <- rec })
}

// holdsSession отвечает, это ли соединение держит сессию. По нему идут и доставка, и поток
// сообщений агента: новое соединение вытесняет прежнее, и получатель всегда тот, кто подключился
// последним.
func (c *Core) holdsSession(sess *Session, conn *Conn) bool {
	return sess.conn == conn
}

// deliveryWait — долгий ответ соединению сессии: слово владельца или команда. Срока у запроса
// нет, он висит, пока службе нечего отдать.
func (c *Core) deliveryWait(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	c.mu.Lock()

	sess := c.session(conn.session)
	if sess == nil || !c.holdsSession(sess, conn) {
		c.mu.Unlock()

		return protocol.Fail(req.RID, protocol.CodeBadRequest,
			"`delivery.wait` зовёт не соединение сессии")
	}

	// Сдвиг курсора приёмом не считается: `after` — память мода о последней принятой доставке, а
	// снимает запись из ящика только `delivery.ack {accepted}`. Честь владельцу ставит подтверждённый
	// приём, а не отправка, поэтому здесь не снимается ничего.
	//
	// Гейт по режиму: со снятым режимом слово из темы сессии не адресовано вовсе, и подключённый
	// мод его не получает. Запись при этом в ящике остаётся — владелец видит глаза, его слово не
	// потеряно, — а уедет она моду тогда, когда режим вернут. Долгий запрос при этом не
	// отказывается, а ждёт: иначе мод крутил бы отказ в цикле, пока владелец не у клавиатуры.
	rec := sess.pendingDelivery(req.After, conn.id)
	if rec != nil && sess.disk.Mode {
		sess.markHanded(rec, conn.id)
		c.mu.Unlock()

		return deliveryResponse(req.RID, rec)
	}

	waiter := &deliveryWaiter{
		connID: conn.id,
		after:  req.After,
		ch:     make(chan *store.Record, 1),
		once:   sync.Once{},
	}
	sess.deliveryWaiters = append(sess.deliveryWaiters, waiter)

	c.mu.Unlock()

	defer c.dropDeliveryWaiter(sess, waiter)

	select {
	case <-ctx.Done():
		// Конец ctx — конец соединения: закрыл его мод или остановка службы. Этот ответ не уходит
		// никому — транспорт не пишет ответов после конца соединения, — и остановка не выглядит
		// для подкоманды `link` отказом.
		return protocol.Fail(req.RID, protocol.CodeBadRequest, "соединение закрылось")
	case got := <-waiter.ch:
		if got == nil {
			return protocol.Fail(req.RID, protocol.CodeBadRequest,
				"соединение больше не держит сессию")
		}

		return deliveryResponse(req.RID, got)
	}
}

// deliveryAck — подтверждение приёма доставки. Запись из ящика снимается только по `accepted`:
// отправка доставки моду приёмом не считается.
func (c *Core) deliveryAck(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	if req.DeliveryID == "" {
		return protocol.Fail(req.RID, protocol.CodeBadRequest, "`delivery.ack` без `delivery_id`")
	}

	c.mu.Lock()

	sess := c.session(conn.session)
	if sess == nil {
		c.mu.Unlock()

		return protocol.Fail(req.RID, protocol.CodeBadRequest, "подтверждение приёма без сессии")
	}

	// Подтверждает приём то же соединение, что держит сессию: вытесненное прежнее иначе снимало
	// бы из ящика запись, которую получило новое, и ставило бы честь за чужой приём.
	if !c.holdsSession(sess, conn) {
		c.mu.Unlock()

		return protocol.Fail(req.RID, protocol.CodeBadRequest,
			"`delivery.ack` зовёт не соединение сессии")
	}

	rec := sess.record(req.DeliveryID)

	c.mu.Unlock()

	if rec == nil {
		// Подтверждение идемпотентно: повторное на ту же запись отвечает `ok` и ничего не меняет.
		return protocol.Ok(req.RID)
	}

	switch req.Outcome {
	case protocol.AckAccepted:
		c.acceptRecord(ctx, sess, rec)
	case protocol.AckRejected:
		c.rejectRecord(ctx, sess, rec, req.Detail)
	default:
		return protocol.Fail(req.RID, protocol.CodeBadRequest, "негодный `outcome` подтверждения")
	}

	return protocol.Ok(req.RID)
}

// acceptRecord снимает запись из ящика и ставит честь — принял агент.
func (c *Core) acceptRecord(ctx context.Context, sess *Session, rec *store.Record) {
	c.mu.Lock()

	if sess.record(rec.ID) == nil {
		c.mu.Unlock()

		return
	}

	sess.dropRecord(rec.ID)

	c.mu.Unlock()
	c.mustSave(rec.Drop(c.store))
	c.journal(store.Entry{
		At: time.Now(), Event: "record-accepted", Session: rec.Session,
		Outcome: string(rec.Delivery), Code: "", Text: rec.ID, TopicID: rec.TopicID,
	})
	c.mark(ctx, sess, rec.MessageID, rec.TopicID, botapi.MarkAccepted)
}

// rejectRecord оставляет запись в ящике и показывает владельцу, что слово придержано: подача у
// агента не разрешилась, значит у него этого слова нет. Причина отказа идёт в журнал службы, а не
// строкой в тему: состояние своего слова владелец читает на самом сообщении.
func (c *Core) rejectRecord(ctx context.Context, sess *Session, rec *store.Record, detail string) {
	c.journal(store.Entry{
		At: time.Now(), Event: "record-rejected", Session: rec.Session,
		Outcome: string(protocol.AckRejected), Code: "", Text: detail, TopicID: rec.TopicID,
	})
	c.hold(ctx, sess, rec)
}

// wakeDelivery будит ждущего, которому эта доставка подходит. Со снятым режимом не будит никого:
// доставка знает о режиме, и слово из темы ждёт в ящике возврата режима. Ждущему, которому эту
// запись уже подавали, она не подаётся снова: приём он не подтвердил, и повтор на том же
// соединении дал бы круг.
func (c *Core) wakeDelivery(sess *Session, rec *store.Record) {
	if !sess.disk.Mode {
		return
	}

	kept := sess.deliveryWaiters[:0]

	for _, waiter := range sess.deliveryWaiters {
		if waiter.after == rec.ID || sess.handed[rec.ID] == waiter.connID {
			kept = append(kept, waiter)

			continue
		}

		sess.markHanded(rec, waiter.connID)
		waiter.resolve(rec)
	}

	sess.deliveryWaiters = kept
}

// wakePending отдаёт ждущему слово владельца, придержанное в ящике: дождавшееся возврата режима
// или не принятое прежним соединением сессии.
func (c *Core) wakePending(sess *Session) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, rec := range sess.inbox {
		if len(sess.deliveryWaiters) == 0 {
			return
		}

		c.wakeDelivery(sess, rec)
	}
}

// dropDeliveryWaiter убирает ждущего, чей запрос кончился.
func (c *Core) dropDeliveryWaiter(sess *Session, waiter *deliveryWaiter) {
	c.mu.Lock()
	defer c.mu.Unlock()

	kept := make([]*deliveryWaiter, 0, len(sess.deliveryWaiters))

	for _, candidate := range sess.deliveryWaiters {
		if candidate != waiter {
			kept = append(kept, candidate)
		}
	}

	sess.deliveryWaiters = kept
}

// deliveryResponse собирает долгий ответ на `delivery.wait`.
func deliveryResponse(rid string, rec *store.Record) protocol.Response {
	author := rec.Author
	resp := protocol.Ok(rid)
	resp.Kind = rec.Delivery
	resp.DeliveryID = rec.ID
	resp.Text = rec.Text
	resp.Author = &author
	resp.Provenance = rec.Provenance
	resp.Topic = &protocol.TopicRef{ID: rec.TopicID, Name: rec.TopicName}

	return resp
}
