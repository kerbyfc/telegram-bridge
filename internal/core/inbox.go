package core

import (
	"context"
	"fmt"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// recordInput — из чего собирается запись ящика.
type recordInput struct {
	delivery  protocol.DeliveryKind
	text      string
	author    protocol.Author
	messageID int
	at        time.Time
}

// enqueue кладёт слово владельца в ящик и будит того, кто его ждёт. Запись держится в ящике,
// пока не придёт подтверждение приёма: отправка доставки моду приёмом не считается.
func (c *Core) enqueue(sess *Session, in recordInput) *store.Record {
	c.mu.Lock()

	seq := c.nextSeq
	c.nextSeq++

	rec := &store.Record{
		Seq:        seq,
		Session:    sess.disk.ID,
		ID:         store.NewID("d", seq),
		Text:       in.text,
		Author:     in.author,
		Provenance: provenance(in.author, in.at),
		MessageID:  in.messageID,
		TopicID:    sess.disk.TopicID,
		TopicName:  sess.disk.TopicName,
		At:         in.at,
		Delivery:   in.delivery,
	}

	err := rec.Validate()
	if err != nil {
		panic(err.Error())
	}

	sess.inbox = append(sess.inbox, rec)
	c.mustSave(rec.Save(c.store))
	c.wakeDelivery(sess, rec)

	c.mu.Unlock()

	c.journal(store.Entry{
		At: time.Now(), Event: "record-queued", Session: rec.Session,
		Outcome: string(rec.Delivery), Code: "", Text: rec.ID, TopicID: rec.TopicID,
	})

	return rec
}

// mark ставит отметку на сообщении владельца в теме topicID: судьбу слова в ящике или исход
// команды темы.
func (c *Core) mark(ctx context.Context, sess *Session, messageID int, topicID int64, what botapi.Mark) {
	if messageID == 0 {
		return
	}

	err := c.withTopic(ctx, sess, workOther, func(_ int64) error {
		return c.api.SetReaction(ctx, messageID, what)
	})
	if err != nil {
		c.journal(store.Entry{
			At: time.Now(), Event: "mark-failed", Session: sess.disk.ID,
			Outcome: string(what), Code: string(botapi.Code(err)), Text: err.Error(),
			TopicID: topicID,
		})
	}
}

// hold — единственное место, где служба читает состояние «слово придержано»: у агента его нет,
// оно ждёт в ящике. Сюда приходят все случаи, которые этим состоянием и являются, — приём не
// подтвердился, соединение сессии пропало, режим снят, — и владелец читает их отметкой на своём
// же сообщении. Строк о неподанных словах в теме нет ни одной: прочитай владелец своё решение
// иначе, правка будет здесь.
func (c *Core) hold(ctx context.Context, sess *Session, records ...*store.Record) {
	for _, rec := range records {
		c.mark(ctx, sess, rec.MessageID, rec.TopicID, botapi.MarkHeld)
	}
}

// provenance собирает готовую строку провенанса, которую мод приклеивает к подаче: кто написал,
// человек или бот, и когда.
func provenance(author protocol.Author, at time.Time) string {
	who := "человек"
	if author.Kind == protocol.AuthorBot {
		who = "бот"
	}

	return fmt.Sprintf("Доставлено из Telegram в %s, автор %s, %s", at.Format("15:04"), author.Name, who)
}
