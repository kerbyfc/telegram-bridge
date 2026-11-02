package link

import (
	"context"

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
func (l *Link) stream(ctx context.Context, message Message) (protocol.Response, error) {
	err := message.validate()
	if err != nil {
		return protocol.Response{}, err
	}

	client, err := l.linked()
	if err != nil {
		return protocol.Response{}, err
	}

	//nolint:exhaustruct // кадр заполняется по операции
	resp, err := client.Call(ctx, protocol.Request{Op: protocol.OpAgentMessage, Text: message.Text})
	if err != nil {
		return protocol.Response{}, err
	}

	return resp, nil
}
