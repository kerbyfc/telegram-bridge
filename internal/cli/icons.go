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

// События журнала о сверке значков при запуске службы: «набор не получен» и «значка нет в наборе» —
// разные вещи, и по журналу они читаются порознь.
const (
	iconsUnverifiedEvent = "topic-icons-unverified"
	iconsOutsideSetEvent = "topic-icons-outside-set"
)

// iconsCheckTimeout — срок сверки значков с набором Telegram. Сверка идёт при запуске службы и в
// разовых командах, а мёртвая сеть не повод держать её до конца всех повторов: у запуска этот срок —
// ещё и предел, на который сверка откладывает приём соединений.
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

// checkIconsAtStart сверяет значки при запуске службы, и подъёма службы эта сверка не решает: адрес
// к этому времени уже поднят, а набор значков спрашивается сетью. Всё, кроме подтверждённой сверки,
// ложится строкой журнала: недоступность Telegram не делает значение негодным, а значок вне набора
// служба всё равно не поставит — отказ у самого значка скажет журнал на первом переключении. Отказом
// сверка отвечает только тогда, когда строку не принял журнал: служба без журнала не работает, и это
// отказ не значка, а состояния.
func checkIconsAtStart(ctx context.Context, client *botapi.Client, cfg *config.Config, jrnl *store.Journal) error {
	verdict := iconsCheck(ctx, client, cfg)

	switch verdict.State {
	case stateOK:
		return nil
	case stateFailed:
		return writeIconsEntry(jrnl, iconsOutsideSetEvent, verdict.Detail)
	case stateUnknown:
		return writeIconsEntry(jrnl, iconsUnverifiedEvent, verdict.Detail)
	default:
		panic("исход сверки значков неизвестен: " + string(verdict.State))
	}
}

// writeIconsEntry кладёт исход сверки в журнал службы.
func writeIconsEntry(jrnl *store.Journal, event, text string) error {
	err := jrnl.Write(store.Entry{
		At: time.Now(), Event: event, Session: "", Outcome: "",
		Code: "", Text: text, TopicID: 0,
	})
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	return nil
}
