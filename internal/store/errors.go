package store

import (
	"errors"
	"fmt"
)

var (
	// ErrBadRecord — запись ящика без обязательного поля.
	ErrBadRecord = errors.New("запись ящика негодна")
	// ErrBadState — состояние на диске негодно.
	ErrBadState = errors.New("состояние негодно")
)

// errReadDir — каталог состояния не прочитан.
func errReadDir(err error) error {
	return fmt.Errorf("%w: каталог состояния не прочитан: %w", ErrBadState, err)
}

// errRemove — файл состояния не убран.
func errRemove(err error) error {
	return fmt.Errorf("%w: файл состояния не убран: %w", ErrBadState, err)
}
