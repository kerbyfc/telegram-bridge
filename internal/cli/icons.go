package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// iconsCheckName — имя проверки значков в самопроверке.
const iconsCheckName = "значки тем"

// iconsCheckTimeout — срок сверки значков с набором Telegram. Сверка стоит перед подъёмом службы и
// в разовых командах, а мёртвая сеть не повод держать их до конца всех повторов.
const iconsCheckTimeout = 5 * time.Second

// iconsCheck сверяет значки из настроек с закрытым набором Telegram. Значка нет в наборе — отказ с
// именем ключа и значением. Набор не получен — «не подтверждено»: о значении это не говорит ничего,
// а про сеть и токен скажут их собственные проверки.
func iconsCheck(ctx context.Context, client *botapi.Client, cfg *config.Config) check {
	ctx, cancel := context.WithTimeout(ctx, iconsCheckTimeout)
	defer cancel()

	icons := []struct{ key, emoji string }{
		{config.TopicIconOnKey, cfg.TopicIconOn},
		{config.TopicIconOffKey, cfg.TopicIconOff},
	}

	missing := make([]string, 0, len(icons))

	for _, icon := range icons {
		known, err := client.TopicIconKnown(ctx, icon.emoji)
		if err != nil {
			return check{
				Name: iconsCheckName, State: stateUnknown,
				Detail: "набор значков Telegram не получен: " + err.Error(),
			}
		}

		if !known {
			missing = append(missing, fmt.Sprintf("`%s`: значка %s нет в наборе значков тем Telegram", icon.key, icon.emoji))
		}
	}

	if len(missing) > 0 {
		return check{Name: iconsCheckName, State: stateFailed, Detail: strings.Join(missing, "; ")}
	}

	return check{
		Name: iconsCheckName, State: stateOK,
		Detail: fmt.Sprintf("вкл %s, выкл %s — есть в наборе Telegram", cfg.TopicIconOn, cfg.TopicIconOff),
	}
}

// configuredIconsCheck — та же сверка по одним настройкам. Клиент не собрался — сверять нечем, и это
// «не подтверждено»: про негодные значения клиента говорит проверка настроек.
func configuredIconsCheck(ctx context.Context, cfg *config.Config) check {
	client, err := newClient(cfg)
	if err != nil {
		return check{Name: iconsCheckName, State: stateUnknown, Detail: err.Error()}
	}

	return iconsCheck(ctx, client, cfg)
}

// verifyIcons отдаёт установке сверку значков: отказ отменяет установку, «не подтверждено» её не
// останавливает и ложится строкой отчёта.
func verifyIcons(ctx context.Context) func(*config.Config) (string, error) {
	return func(cfg *config.Config) (string, error) {
		verdict := configuredIconsCheck(ctx, cfg)
		if verdict.State == stateFailed {
			return "", errors.New(verdict.Detail)
		}

		return verdict.Detail, nil
	}
}

// checkIconsAtStart сверяет значки перед подъёмом службы. Значка нет в наборе — служба не поднимается,
// как при всяких негодных настройках. Набор не получен — служба поднимается: недоступность Telegram
// не делает значение негодным, а отказ у самого значка скажет журнал на первом переключении.
func checkIconsAtStart(ctx context.Context, client *botapi.Client, cfg *config.Config, jrnl *store.Journal) error {
	verdict := iconsCheck(ctx, client, cfg)

	switch verdict.State {
	case stateOK:
		return nil
	case stateFailed:
		return exitError{ExitUsage, verdict.Detail}
	case stateUnknown:
		err := jrnl.Write(store.Entry{
			At: time.Now(), Event: "topic-icons-unverified", Session: "", Outcome: "",
			Code: "", Text: verdict.Detail, TopicID: 0,
		})
		if err != nil {
			return exitError{ExitFailed, err.Error()}
		}

		return nil
	default:
		panic("исход сверки значков неизвестен: " + string(verdict.State))
	}
}
