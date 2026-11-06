package core

import (
	"context"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/resolve"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// UnresolvedPrefix — начало строки, которая встаёт в тему вместо вывода упавшей команды
// разворота: тихого пропуска нет, владелец видит ссылку и причину.
const UnresolvedPrefix = "ссылка не развёрнута: "

// resolution — разворот одной ссылки, запущенный в момент прихода реплики.
type resolution struct {
	link string
	out  chan resolved
}

// resolved — исход команды разворота.
type resolved struct {
	text string
	err  error
}

// startResolving находит ссылки реплики по записям настроек и сразу запускает их команды: пока
// уходит сама реплика, команды уже работают. Отменой звонящий снимает команды, чей вывод
// больше не нужен.
func (c *Core) startResolving(ctx context.Context, text string) ([]resolution, context.CancelFunc) {
	hits := resolve.Find(text, c.cfg.LinkResolvers)

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	jobs := make([]resolution, 0, len(hits))

	for _, hit := range hits {
		job := resolution{link: hit.Link, out: make(chan resolved, 1)}
		jobs = append(jobs, job)

		go func() {
			out, err := resolve.Run(runCtx, hit.Resolver, hit.Link)
			job.out <- resolved{text: out, err: err}
		}()
	}

	return jobs, cancel
}

// deliverResolved отправляет вывод разворота следом за репликой, по порядку ссылок в тексте,
// тем же резаком по пределу Telegram. Упавшая команда даёт строку отказа с причиной. Конец —
// закрытие `done`: следующая реплика сессии ждёт его, чтобы не встать между репликой и её
// выводом.
func (c *Core) deliverResolved(
	ctx context.Context, sess *Session, jobs []resolution, cancel context.CancelFunc, done chan struct{},
) {
	defer close(done)
	defer cancel()

	for _, job := range jobs {
		res := <-job.out
		text := res.text

		if res.err != nil {
			text = UnresolvedPrefix + job.link + ": " + res.err.Error()
			c.journalResolve(sess, "link-unresolved", job.link+": "+res.err.Error())
		}

		c.topicMu.Lock()
		_, err := c.sendChunks(ctx, sess, text)
		c.topicMu.Unlock()

		if err != nil {
			c.journalResolve(sess, "link-send-failed", job.link+": "+err.Error())
		}
	}
}

// journalResolve пишет строку журнала о развороте ссылки.
func (c *Core) journalResolve(sess *Session, event, text string) {
	c.mu.Lock()
	session := sess.disk.ID
	topicID := sess.disk.TopicID
	c.mu.Unlock()

	c.journal(store.Entry{
		At: time.Now(), Event: event, Session: session, Outcome: "", Code: "", Text: text, TopicID: topicID,
	})
}
