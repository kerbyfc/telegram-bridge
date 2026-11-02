// telegram-bridge — мост Telegram: служба, которую держит система, и тонкий клиент
// сессии одним бинарником.
//
//	telegram-bridge serve    держать службу: адрес границы, опрос обновлений, сторож живости
//
// Настройки — свой `config.yaml`, путь к нему в `--config` или `DM_TELEGRAM_CONFIG`.
// Протокол и состояние — `contract.md`.
package main

import (
	"os"

	"github.com/kerbyfc/telegram-bridge/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
