package link

import (
	"context"
	"fmt"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// Turn — ход, который средство сессии называет на его границах. Имя хода — его id у клиента, и
// нужен он для порядка: извещения ответа не ждут, поэтому конец прошлого хода может прийти после
// начала следующего, и служба по этому имени гасит «печатает» только своего хода. Имя
// необязательное — конец без имени гасит текущий ход.
type Turn struct {
	ID string `json:"turn,omitempty"`
}

// Report — процент занятого окна контекста, как его называет средство сессии. Своего счёта
// контекста ни подкоманда, ни служба не ведут: число знает клиент, у которого окно и считается,
// и протокол несёт его одним полем.
type Report struct {
	Percent int `json:"percent"`
}

// validate проверяет процент: доля окна лежит в сотне, и отрицательной она не бывает. Негодное
// число — отказ с названной причиной, а не правка строки состояния чем попало.
func (r Report) validate() error {
	if r.Percent < 0 || r.Percent > 100 {
		return fmt.Errorf("%w: %d", ErrBadReport, r.Percent)
	}

	return nil
}

// notify проводит в службу извещение о границе хода. Ответа оно по контракту не ждёт, но ответ
// службы доходит до средства сессии как есть: отказ канала оно обязано увидеть, а не проглотить.
func (l *Link) notify(ctx context.Context, op protocol.Op, turn Turn) (protocol.Response, error) {
	client, err := l.linked()
	if err != nil {
		return protocol.Response{}, err
	}

	//nolint:exhaustruct // кадр заполняется по операции
	resp, err := client.Call(ctx, protocol.Request{Op: op, Turn: turn.ID})
	if err != nil {
		return protocol.Response{}, err
	}

	return resp, nil
}

// report проводит процент контекста в службу. Как часто правится закреплённая строка и правится
// ли она вообще, решает служба: при снятом режиме строка состояния темы не живёт, и число остаётся
// у службы до первой границы хода в вернувшемся режиме.
func (l *Link) report(ctx context.Context, report Report) (protocol.Response, error) {
	err := report.validate()
	if err != nil {
		return protocol.Response{}, err
	}

	client, err := l.linked()
	if err != nil {
		return protocol.Response{}, err
	}

	//nolint:exhaustruct // кадр заполняется по операции
	resp, err := client.Call(ctx, protocol.Request{Op: protocol.OpContextReport, Percent: report.Percent})
	if err != nil {
		return protocol.Response{}, err
	}

	return resp, nil
}
