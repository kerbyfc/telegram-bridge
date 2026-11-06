package cli

import (
	"slices"
	"strings"

	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// resolversCheck называет программы записей разворота ссылок так, как их нашла служба: по её
// PATH, а не по PATH терминала, где идёт самопроверка, — под автозапуском он обычно короче.
// Путь называет сама служба ответом `service.status`; программа, которую она не нашла, службу не
// поднимает, и такую причину называет проверка службы хвостом её вывода. Службы нет — пути не
// узнать, и подменять PATH службы терминальным проверка не берётся.
func resolversCheck(cfg *config.Config, status *protocol.ServiceStatus) check {
	if status == nil {
		return check{
			Name: "ссылки", State: stateUnknown,
			Detail: "службы нет: программы ищет служба по своему PATH, а PATH терминала за него не отвечает",
		}
	}

	named := make([]string, 0, len(cfg.LinkResolvers))
	for _, resolver := range cfg.LinkResolvers {
		named = append(named, resolver.Program)
	}

	found := make([]string, 0, len(status.Resolvers))
	served := make([]string, 0, len(status.Resolvers))

	for _, resolver := range status.Resolvers {
		found = append(found, resolver.Program)
		served = append(served, resolver.Program+" → "+resolver.Path)
	}

	if !slices.Equal(named, found) {
		return check{
			Name: "ссылки", State: stateUnknown,
			Detail: "служба поднята с другими записями, чем в файле настроек: перезапусти службу; служба нашла: " +
				orNone(served),
		}
	}

	return check{Name: "ссылки", State: stateOK, Detail: "служба нашла: " + strings.Join(served, "; ")}
}

// orNone отдаёт перечень через «; » или «ничего».
func orNone(items []string) string {
	if len(items) == 0 {
		return "ничего"
	}

	return strings.Join(items, "; ")
}
