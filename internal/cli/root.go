// Package cli — команды `telegram-bridge` на cobra; настройки — viper по своему файлу
// `config.yaml`, путь к которому берётся из флага или из `DM_TELEGRAM_CONFIG`. Общая цепочка
// окружения деревьев здесь не повторяется: у службы один источник настроек.
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/kerbyfc/telegram-bridge/internal/config"
)

// ExitUsage — негодный вызов или непрочитанные настройки.
const ExitUsage = 2

// ExitFailed — служба не поднялась или кончилась отказом.
const ExitFailed = 1

// exitError несёт код возврата команды.
type exitError struct {
	code int
	msg  string
}

// Error делает код возврата обычной ошибкой Go.
func (e exitError) Error() string {
	return e.msg
}

// Run выполняет команду и возвращает код возврата процесса.
func Run(args []string) int {
	root := newRoot()
	root.SetArgs(args)

	err := root.Execute()
	if err == nil {
		return 0
	}

	var exit exitError
	if errors.As(err, &exit) {
		if exit.msg != "" {
			fmt.Fprintln(os.Stderr, "telegram-bridge: "+exit.msg)
		}

		return exit.code
	}

	fmt.Fprintln(os.Stderr, "telegram-bridge: "+err.Error())

	return ExitUsage
}

// newRoot собирает дерево команд.
func newRoot() *cobra.Command {
	vip := viper.New()
	root := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:           "telegram-bridge",
		Short:         "Мост Telegram: служба, тонкий клиент сессии",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("config", "", "путь к `config.yaml` службы ("+config.EnvConfig+")")
	_ = vip.BindPFlag("config", root.PersistentFlags().Lookup("config"))
	_ = vip.BindEnv("config", config.EnvConfig)
	root.AddCommand(newServe(vip))
	root.AddCommand(newLink(vip))
	root.AddCommand(newInstall(vip))
	root.AddCommand(newUninstall(vip))
	root.AddCommand(newAway(vip))
	root.AddCommand(newSay(vip))
	root.AddCommand(newAsk(vip))
	root.AddCommand(newHook(vip))
	root.AddCommand(newDoctor(vip))
	root.AddCommand(newImportTopics(vip))

	return root
}

// loadConfig читает настройки службы по флагу или окружению.
func loadConfig(vip *viper.Viper) (*config.Config, error) {
	path := vip.GetString("config")
	if path == "" {
		return nil, exitError{ExitUsage, "путь к настройкам не назван: --config или " + config.EnvConfig}
	}

	cfg, err := config.Load(path)
	if err != nil {
		return nil, exitError{ExitUsage, err.Error()}
	}

	return cfg, nil
}
