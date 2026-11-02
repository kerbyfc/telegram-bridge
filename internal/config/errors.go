package config

import "errors"

var (
	// ErrBadConfig — в настройках нет обязательного ключа или значение негодное.
	ErrBadConfig = errors.New("настройки негодны")
	// ErrNoOwner — нет `owner_id`: служба не поднимается, иначе любой участник группы получал
	// бы ввод уровня владельца.
	ErrNoOwner = errors.New("в настройках нет owner_id")
)
