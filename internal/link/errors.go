package link

import (
	"errors"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// RefusedError — отказ службы с кодом контракта. Код доходит до средства сессии как есть:
// тихого успеха на ошибке нет ни у одной стороны.
type RefusedError struct {
	Code protocol.Code
	Text string

	reason error
}

// Error называет отказ службы её же словами.
func (r RefusedError) Error() string {
	return r.reason.Error() + " `" + string(r.Code) + "`: " + r.Text
}

// Unwrap отдаёт причину, по которой новое соединение делу не поможет.
func (r RefusedError) Unwrap() error {
	return r.reason
}

// ErrVersion — служба другой мажорной версии контракта. Повторять соединение незачем: владелец
// узнаёт о расхождении в терминале, а не по пропавшему каналу.
var ErrVersion = errors.New("служба другой мажорной версии контракта")

// ErrRefused — служба отказала на кадре, который обязана была принять: негодное приветствие или
// вытесненное соединение сессии. Новое соединение тут делу не поможет и только отбирало бы
// сессию у того мода, который её уже держит.
var ErrRefused = errors.New("служба отказала")

// ErrLinkLost — соединение со службой пропало.
var ErrLinkLost = errors.New("соединение со службой потеряно")

// ErrNoLink — средство сессии зовёт подкоманду, когда соединения со службой нет.
var ErrNoLink = errors.New("соединения со службой нет")

// ErrNotHanded — подтверждение приёма пришло не на ту доставку, которую подкоманда выдала.
var ErrNotHanded = errors.New("подтверждение не на выданную доставку")

// ErrBadAck — в подтверждении приёма нет обязательного поля или у него негодное значение.
var ErrBadAck = errors.New("негодное подтверждение приёма")

// ErrBadReport — процент контекста вне сотни: доля окна отрицательной не бывает и сотни не
// перебирает.
var ErrBadReport = errors.New("негодный процент контекста")

// ErrEmptyMessage — реплика агента без текста: отправлять в тему нечего.
var ErrEmptyMessage = errors.New("реплика агента без текста")
