package link

import (
	"context"
	"errors"
	"fmt"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// Message — дописанная реплика агента, как средство сессии отдаёт её подкоманде: текст целиком и
// одним куском. Кусков на лету здесь не бывает — средство сессии зовёт этот вход, только когда
// реплика дописана, и делит речь агента от вывода инструментов тот, кто их видит.
type Message struct {
	Text string `json:"text"`
}

// validate проверяет реплику: пустой текст в тему не уходит — отправлять нечего, а сообщение без
// текста Bot API и не примет.
func (m Message) validate() error {
	if m.Text == "" {
		return ErrEmptyMessage
	}

	return nil
}

// stream проводит дописанную реплику в службу. Ответа средство сессии не ждёт, но ответ службы
// доходит до него как есть: отказ канала оно обязано увидеть, а не проглотить. Нарезку по пределу
// сообщения Telegram делает служба — у подкоманды своего мерила предела нет.
//
// Связи со службой нет или она пропала на этой реплике — реплика встаёт в очередь и уходит
// вернувшимся соединением в том же порядке; ответ средству сессии тогда — удача: реплика у
// подкоманды, и о том, что она так и не дошла, скажет событие `undelivered`.
func (l *Link) stream(ctx context.Context, message Message) (protocol.Response, error) {
	err := message.validate()
	if err != nil {
		return protocol.Response{}, err
	}

	client, queued := l.streamTo(message.Text)
	if queued {
		return protocol.Ok(""), nil
	}

	//nolint:exhaustruct // кадр заполняется по операции
	resp, err := client.Call(ctx, protocol.Request{Op: protocol.OpAgentMessage, Text: message.Text})
	if errors.Is(err, ErrLinkLost) {
		l.queue(message.Text, true)

		return protocol.Ok(""), nil
	}

	if err != nil {
		return protocol.Response{}, err
	}

	return resp, nil
}

// streamTo отдаёт соединение для реплики или ставит её в очередь: без связи и тогда, когда
// очередь ещё не отдана, — новая реплика не обгоняет старые. Решение и постановка идут под одним
// замком: иначе реплика встала бы в очередь, которую отдача уже кончила.
func (l *Link) streamTo(text string) (*Client, bool) {
	l.mu.Lock()

	client := l.client
	if client != nil && !l.flushing && len(l.backlog) == 0 {
		l.mu.Unlock()

		return client, false
	}

	lost := l.enqueue(text, false)
	l.mu.Unlock()

	l.overflow(lost)
	l.wake()

	return nil, true
}

// queue кладёт в начало очереди реплику, оборванную на лету: она старше ждущих.
func (l *Link) queue(text string, first bool) {
	l.mu.Lock()
	lost := l.enqueue(text, first)
	l.mu.Unlock()

	l.overflow(lost)
	l.wake()
}

// enqueue кладёт реплику в очередь под замком и отвечает, потерялась ли старейшая сверх предела.
func (l *Link) enqueue(text string, first bool) bool {
	if first {
		l.backlog = append([]string{text}, l.backlog...)
	} else {
		l.backlog = append(l.backlog, text)
	}

	if len(l.backlog) <= maxBacklog {
		return false
	}

	l.backlog = l.backlog[1:]

	return true
}

// overflow говорит владельцу о реплике, потерянной сверх предела очереди.
func (l *Link) overflow(lost bool) {
	if lost {
		l.undelivered(fmt.Sprintf("связи со службой нет, а копится не больше %d реплик", maxBacklog), 1)
	}
}

// flusher отдаёт накопленные реплики, пока жива подкоманда: будит его вернувшееся соединение и
// каждая реплика, вставшая в очередь. Отдатчик один, поэтому порядок реплик не ломает ни гонка
// двух соединений, ни реплика, пришедшая посреди отдачи.
func (l *Link) flusher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.kick:
		}

		l.flush(ctx)
	}
}

// wake будит отдатчик очереди.
func (l *Link) wake() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// flush отдаёт очередь текущим соединением по одной реплике, в порядке прихода. Связь пропала
// снова — реплика возвращается в начало очереди и ждёт следующего соединения.
func (l *Link) flush(ctx context.Context) {
	for {
		l.mu.Lock()

		client := l.client
		if client == nil || len(l.backlog) == 0 {
			l.flushing = false
			l.mu.Unlock()

			return
		}

		l.flushing = true
		text := l.backlog[0]
		l.backlog = l.backlog[1:]
		l.mu.Unlock()

		//nolint:exhaustruct // кадр заполняется по операции
		resp, err := client.Call(ctx, protocol.Request{Op: protocol.OpAgentMessage, Text: text})
		if err != nil {
			l.mu.Lock()
			lost := l.enqueue(text, true)
			l.flushing = false
			l.mu.Unlock()

			l.overflow(lost)

			return
		}

		if !resp.OK {
			l.refused(resp)
		}
	}
}

// refused решает, что делать с репликой из очереди, которую служба отбила. Снятый режим — обычное
// дело: вне режима реплики в тему не идут вовсе, и говорить владельцу не о чем. Прочий отказ —
// реплика потеряна, и владелец узнаёт об этом.
func (l *Link) refused(resp protocol.Response) {
	failure := refusal(resp)

	var code RefusedError
	if errors.As(failure, &code) && code.Code == protocol.CodeNoMode {
		l.note("реплика из очереди не ушла: режим снят")

		return
	}

	// Владельцу — код отказа; полный текст причины идёт в поток ошибок, в журнал отладки.
	l.note("служба отбила реплику из очереди: " + failure.Error())
	l.undelivered("служба отказала `"+string(code.Code)+"`", 1)
}

// abandon говорит о репликах, которые так и не дождались связи: подкоманда кончается.
func (l *Link) abandon() {
	l.mu.Lock()
	count := len(l.backlog)
	l.backlog = nil
	l.mu.Unlock()

	if count > 0 {
		l.undelivered("подкоманда кончилась без связи со службой", count)
	}
}

// undelivered говорит средству сессии, что реплики агента до темы не дошли.
func (l *Link) undelivered(why string, count int) {
	text := fmt.Sprintf("реплик агента не доставлено в тему: %d — %s", count, why)

	l.note(text)
	l.emit(Event{Event: EventUndelivered, Text: text}) //nolint:exhaustruct // поля события задаёт его вид
}
