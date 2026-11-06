package core

import (
	"context"
	"fmt"
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

// batch — развороты ссылок одной реплики. Место отправщика в счёте горутин занято заранее: его
// забирает либо отправка вывода, либо отказ от неё.
type batch struct {
	jobs   []resolution
	cancel context.CancelFunc
}

// startResolving находит ссылки реплики по записям настроек и сразу запускает их команды: пока
// уходит сама реплика, команды уже работают. Остановленная служба команд не запускает, а каждую
// ссылку называет строкой журнала `link-dropped`.
func (c *Core) startResolving(sess *Session, text string) *batch {
	hits := resolve.Find(text, c.cfg.LinkResolvers)
	if len(hits) == 0 {
		return nil
	}

	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()

		for _, hit := range hits {
			c.journalResolve(sess, "link-dropped", hit.Link+": служба остановлена")
		}

		return nil
	}

	c.resolving.Add(len(hits) + 1)
	c.mu.Unlock()

	runCtx, cancel := context.WithCancel(c.resolveCtx)
	work := &batch{jobs: make([]resolution, 0, len(hits)), cancel: cancel}

	for _, hit := range hits {
		job := resolution{link: hit.Link, out: make(chan resolved, 1)}
		work.jobs = append(work.jobs, job)

		go func() {
			defer c.resolving.Done()

			out, err := resolve.Run(runCtx, hit.Resolver, hit.Link)
			job.out <- resolved{text: out, err: err}
		}()
	}

	return work
}

// abandon снимает команды реплики, вывод которых ставить некуда: реплика в тему не легла.
func (c *Core) abandon(work *batch) {
	if work == nil {
		return
	}

	work.cancel()
	c.resolving.Done()
}

// deliverResolved отправляет вывод разворота следом за репликой, по порядку ссылок в тексте,
// тем же резаком по пределу Telegram. Упавшая команда даёт строку отказа с причиной; остановка
// службы — строку журнала `link-dropped` вместо сообщения. Конец — закрытие `done`: следующее
// сообщение сессии ждёт его, чтобы не встать между репликой и её выводом.
func (c *Core) deliverResolved(sess *Session, work *batch, done chan struct{}) {
	defer c.resolving.Done()
	defer close(done)
	defer work.cancel()

	for _, job := range work.jobs {
		res := <-job.out

		if c.resolveCtx.Err() != nil {
			c.journalResolve(sess, "link-dropped", job.link+": служба остановлена")

			continue
		}

		text := res.text

		if res.err != nil {
			text = UnresolvedPrefix + job.link + ": " + res.err.Error()
			c.journalResolve(sess, "link-unresolved", job.link+": "+res.err.Error())
		}

		c.topicMu.Lock()
		_, err := c.sendChunks(c.resolveCtx, sess, text)
		c.topicMu.Unlock()

		if err != nil {
			c.journalResolve(sess, "link-send-failed", job.link+": "+err.Error())
		}
	}
}

// enterChain ставит сообщение сессии в очередь темы: ждёт, пока ляжет всё от прошлого —
// реплика и вывод разворота её ссылок, — и отдаёт свой знак конца, который звонящий закрывает,
// когда его сообщение легло. Ожидание конечно: команды ограничены своими сроками, отправка —
// сроками Bot API, — и ограничено ctx запроса: кончился срок дописывания на остановке службы —
// сообщение не отправляется, а его место в очереди освобождается, когда ляжет прошлое.
func (c *Core) enterChain(ctx context.Context, sess *Session) (chan struct{}, error) {
	done := make(chan struct{})

	c.mu.Lock()
	prev := sess.replied
	sess.replied = done
	c.mu.Unlock()

	if prev == nil {
		return done, nil
	}

	select {
	case <-prev:
		return done, nil
	case <-ctx.Done():
		go func() {
			<-prev
			close(done)
		}()

		return nil, fmt.Errorf("очередь темы не дождалась прошлого сообщения: %w", ctx.Err())
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
