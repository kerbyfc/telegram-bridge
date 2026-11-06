package cli

import (
	"os/exec"
	"strconv"
	"strings"

	"github.com/kerbyfc/telegram-bridge/internal/config"
)

// resolversCheck проверяет записи разворота ссылок: программа каждой команды находится, исполнима
// и не пакетный файл Windows. Иначе каждая ссылка давала бы в теме строку отказа, а узнать причину заранее было бы
// негде.
func resolversCheck(cfg *config.Config) check {
	missing := make([]string, 0, len(cfg.LinkResolvers))

	for _, resolver := range cfg.LinkResolvers {
		_, err := exec.LookPath(resolver.Command[0])
		if err == nil {
			err = config.CheckProgram(resolver.Command[0])
		}

		if err != nil {
			missing = append(missing, resolver.Command[0]+": "+err.Error())
		}
	}

	if len(missing) > 0 {
		return check{Name: "ссылки", State: stateFailed, Detail: strings.Join(missing, "; ")}
	}

	return check{
		Name: "ссылки", State: stateOK,
		Detail: "записей разворота " + strconv.Itoa(len(cfg.LinkResolvers)) + ", команды находятся",
	}
}
