package link

import (
	"context"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// Name — имя, которое средство сессии называет службе: название чата. Служба не спрашивает,
// откуда имя взялось и чат ли это: ей передают обычное имя, и привязанная тема всегда идёт за ним.
// Пустое имя значит, что названия у чата нет.
type Name struct {
	Name string `json:"name"`
}

// name проводит имя в службу. Ответа извещение не ждёт, но ответ службы доходит до средства сессии
// как есть: отказ канала оно обязано увидеть, а не проглотить.
func (l *Link) name(ctx context.Context, name Name) (protocol.Response, error) {
	client, err := l.linked()
	if err != nil {
		return protocol.Response{}, err
	}

	//nolint:exhaustruct // кадр заполняется по операции
	resp, err := client.Call(ctx, protocol.Request{Op: protocol.OpNameReport, Name: name.Name})
	if err != nil {
		return protocol.Response{}, err
	}

	return resp, nil
}
