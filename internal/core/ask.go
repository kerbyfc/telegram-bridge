package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// questionAsk — второй вход: вопрос владельцу с вариантами от инструмента клиента. Служба
// отправляет его в тему пронумерованным и отвечает сразу: ждать ответа больше нечем, потому что
// ожидания хода у службы нет.
//
// Ответ владельца придёт обычной доставкой и разбудит сессию подачей промпта — номер варианта
// понимает сам агент, он же его и предложил. Разбора номера в ключ варианта у службы нет:
// сопоставлять его не с чем.
//
// Отправка оставляет строку журнала: `question-sent` с текстом, ушедшим в тему, и id его
// сообщений, отказ — `question-failed`. Что владелец увидел в теме, иначе не сверить: в журнал
// не попадает больше ничего о вопросе, а Bot API прочесть тему не даёт.
func (c *Core) questionAsk(ctx context.Context, conn *Conn, req protocol.Request) protocol.Response {
	if req.Question == "" {
		return protocol.Fail(req.RID, protocol.CodeBadRequest, "`question.ask` без `question`")
	}

	sess, fail := c.sessionInMode(conn, req)
	if fail != nil {
		return *fail
	}

	c.topicMu.Lock()
	defer c.topicMu.Unlock()

	text := askText(req.Question, req.Options)

	ids, err := c.sendChunks(ctx, sess, text)

	// Тему отправка могла завести заново: в журнал идёт та, куда вопрос ушёл.
	c.mu.Lock()

	topicID := sess.disk.TopicID

	c.mu.Unlock()

	if err != nil {
		refusal := refusalOf(err)

		c.journal(store.Entry{
			At: time.Now(), Event: "question-failed", Session: sess.disk.ID,
			Outcome: messageIDs(ids), Code: string(refusal.Code), Text: refusal.Text, TopicID: topicID,
		})

		return failure(req.RID, err)
	}

	c.journal(store.Entry{
		At: time.Now(), Event: "question-sent", Session: sess.disk.ID,
		Outcome: messageIDs(ids), Code: "", Text: text, TopicID: topicID,
	})

	resp := protocol.Ok(req.RID)
	resp.MessageID = lastID(ids)
	resp.MessageIDs = ids

	return resp
}

// askText собирает текст вопроса с пронумерованными вариантами. Нумерация — работа службы, а не
// звонящего: так любой инструмент спрашивает одинаково.
func askText(body string, options []protocol.Option) string {
	if len(options) == 0 {
		return body
	}

	var out strings.Builder

	out.WriteString(body)

	for num, option := range options {
		out.WriteString(fmt.Sprintf("\n%d) %s — %s", num+1, option.Key, option.Text))
	}

	return out.String()
}

// messageIDs называет id сообщений вопроса для журнала: `message_ids=12,13`; не ушло ни одного —
// пустая строка.
func messageIDs(ids []int) string {
	if len(ids) == 0 {
		return ""
	}

	named := make([]string, 0, len(ids))
	for _, id := range ids {
		named = append(named, strconv.Itoa(id))
	}

	return "message_ids=" + strings.Join(named, ",")
}

// lastID отдаёт последний id отправленного сообщения; их нет — нуль.
func lastID(ids []int) int {
	if len(ids) == 0 {
		return 0
	}

	return ids[len(ids)-1]
}
