package core

import (
	"errors"
	"fmt"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// ErrBadState — состояние на диске не сходится само с собой.
var ErrBadState = errors.New("состояние службы негодно")

// errOrphanRecord — запись ящика без привязки сессии: тихо выбросить её значит потерять слово
// владельца, поэтому служба на таком состоянии не поднимается.
func errOrphanRecord(session, id string) error {
	return fmt.Errorf("%w: запись ящика %s сессии %s без привязки", ErrBadState, id, session)
}

// errTopic переводит отказ Bot API в отказ службы с кодом контракта.
func errTopic(err error) error {
	if err == nil {
		return nil
	}

	var refusal *protocol.RefusalError
	if errors.As(err, &refusal) {
		return refusal
	}

	return &protocol.RefusalError{Code: botapi.Code(err), Text: err.Error()}
}

// failure превращает отказ в кадр ответа.
func failure(rid string, err error) protocol.Response {
	refusal := refusalOf(err)

	return protocol.Fail(rid, refusal.Code, refusal.Text)
}

// refusalOf — отказ службы с кодом контракта из ошибки. Ошибка без кода — нарушенный инвариант
// службы: все пути отказа обязаны называть код, и тихого успеха на ошибке нет.
func refusalOf(err error) *protocol.RefusalError {
	var refusal *protocol.RefusalError
	if errors.As(err, &refusal) {
		return refusal
	}

	panic("отказ службы без кода контракта: " + err.Error())
}
