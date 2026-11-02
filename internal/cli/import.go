package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/legacy"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
	"github.com/kerbyfc/telegram-bridge/internal/tool"
)

// flagFrom — каталог состояния старого моста; умолчания нет, флаг обязателен.
const flagFrom = "from"

// flagCheck — сверка без записи: ею владелец проверяет уже перенесённое, ничего не трогая.
const flagCheck = "check"

// importAnswer — машинный отчёт переноса.
type importAnswer struct {
	From    string       `json:"from"`
	Check   bool         `json:"check"`
	Read    int          `json:"read"`
	Placed  int          `json:"placed"`
	Skipped []skipAnswer `json:"skipped"`
	Offset  offsetAnswer `json:"offset"`
}

// offsetAnswer — перенос смещения опроса: откуда оно читалось, каким было у старого моста (нуль —
// его нет) и у службы до переноса, и что с ним сделано.
type offsetAnswer struct {
	From    string `json:"from"`
	Old     int64  `json:"old"`
	Service int64  `json:"service"`
	Outcome string `json:"outcome"`
}

// skipAnswer — пропущенная тема: её имя, id и причина пропуска словами.
type skipAnswer struct {
	Name   string `json:"name"`
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// newImportTopics собирает разовый перенос реестра тем из состояния Python-моста.
func newImportTopics(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "import-topics",
		Short: "перенести реестр тем и смещение опроса из состояния Python-моста (разово, при снятой службе)",
		Long: "Читает реестр тем старого моста — имя темы и её id — и кладёт его в состояние " +
			"службы. Старое состояние только читается: ни файла, ни каталога команда не трогает. " +
			"Каталог старого моста называет `--from`: умолчания у него нет.\n\n" +
			"Вместе с реестром переносится смещение опроса обновлений, и служба начинает с него " +
			"первый опрос: обновление, которое старый мост разобрал, но не успел подтвердить, второй " +
			"раз не приходит. Смещения в старом состоянии нет — переносить нечего; битое — отказ до " +
			"записи. Назад смещение не идёт: у службы, ушедшей дальше, оно остаётся.\n\n" +
			"Перенос идемпотентен: уже перенесённая тема второго запуска не боится, а имя или id, " +
			"занятые в новом реестре, не затираются — такой пропуск называется строкой отчёта. " +
			"Тема, которой в Telegram уже нет, переносится как есть: списка тем платформа не " +
			"отдаёт, а пропавшую тему служба заводит заново при первом обращении к ней. Имя длиннее " +
			"предела Telegram режется тем же правилом, что и в остальном реестре, а значок темы " +
			"записывается неизвестным: первое переключение режима выровняет его.\n\n" +
			"При идущей службе команда отказывается: реестр тем она держит в памяти и перепишет " +
			"файл при первом изменении, поэтому положенное поверх неё пропало бы. Ключ `--check` " +
			"называет, чего в новом реестре не хватает, и ничего не пишет.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImportTopics(cmd, vip)
		},
	}

	cmd.Flags().String(flagFrom, "", "каталог состояния старого моста (обязателен)")
	cmd.Flags().Bool(flagCheck, false, "только сверить перенесённое: ничего не писать")
	machineFlag(cmd)

	return cmd
}

// runImportTopics переносит реестр тем старого моста в состояние службы и называет исход: сколько
// тем прочитано, сколько положено и какие пропущены с причиной.
func runImportTopics(cmd *cobra.Command, vip *viper.Viper) error {
	from := text(cmd, flagFrom)
	if from == "" {
		return exitError{ExitUsage, "каталог состояния старого моста не назван: " +
			"`--from` обязателен, умолчания у него нет"}
	}

	path := vip.GetString("config")

	cfg, err := loadConfig(vip)
	if err != nil {
		return err
	}

	err = refuseLiveService(cmd, cfg, path)
	if err != nil {
		return err
	}

	registryPath := filepath.Join(from, legacy.TopicsFile)
	offsetPath := filepath.Join(from, legacy.OffsetFile)

	err = refuseOwnState(cfg, registryPath)
	if err != nil {
		return err
	}

	// Старое состояние разбирается целиком до записи: битое смещение отбивает перенос так же, как
	// битый реестр, и половины перенесённого не остаётся.
	registry, err := legacy.ReadTopics(registryPath)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	offset, err := legacy.ReadOffset(offsetPath)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	answer, err := portState(cfg, flagBool(cmd, flagCheck), registry, offset)
	if err != nil {
		return err
	}

	answer.From = registryPath
	answer.Offset.From = offsetPath

	return emit(os.Stdout, machine(cmd), answer, importLines(answer)...)
}

// portState кладёт прочитанное старое состояние в состояние службы и отдаёт отчёт без путей
// источника. Писать нечего — не пишем: файл не пересоздаётся, и «повторный запуск ничего не меняет»
// остаётся верным не только про содержимое, но и про сам файл.
func portState(cfg *config.Config, check bool, registry legacy.Registry, offset int64) (importAnswer, error) {
	st, err := store.Open(cfg.StateRoot)
	if err != nil {
		return importAnswer{}, exitError{ExitFailed, err.Error()}
	}

	topics, err := store.LoadTopics(st)
	if err != nil {
		return importAnswer{}, exitError{ExitFailed, err.Error()}
	}

	updates, err := store.LoadUpdates(st)
	if err != nil {
		return importAnswer{}, exitError{ExitFailed, err.Error()}
	}

	report := registry.Port(topics)
	before := updates.Offset
	moved := legacy.PortOffset(offset, updates)

	if !check && report.Placed > 0 {
		err = topics.Save(st)
		if err != nil {
			return importAnswer{}, exitError{ExitFailed, "реестр тем не записан: " + err.Error()}
		}
	}

	if !check && moved == legacy.OffsetPlaced {
		err = updates.Save(st)
		if err != nil {
			return importAnswer{}, exitError{ExitFailed, "смещение опроса не записано: " + err.Error()}
		}
	}

	answer := importAnswerOf(check, report)
	answer.Offset = offsetAnswer{From: "", Old: offset, Service: before, Outcome: string(moved)}

	return answer, nil
}

// refuseLiveService отказывает при идущей службе. Реестр тем служба держит в памяти и переписывает
// файл при каждом изменении, поэтому запись поверх неё пропала бы при первом же её сохранении —
// тихо и вместе с последним перенесённым.
//
// Снятой служба считается только тогда, когда соединения нет вовсе: молчащая живая служба держит
// реестр в памяти так же, как говорящая, и две секунды её молчания — не повод писать поверх неё.
func refuseLiveService(cmd *cobra.Command, cfg *config.Config, path string) error {
	resp, err := tool.Call(cmd.Context(),
		tool.Options{Endpoint: cfg.Endpoint(), Session: "", Timeout: tool.DialTimeout},
		protocol.Request{Op: protocol.OpServiceStatus}) //nolint:exhaustruct // вопросу о службе хватает операции

	switch {
	case errors.Is(err, tool.ErrNoService):
		// Сокета нет или за ним никто не слушает: службы нет, переносить можно.
		return nil
	case errors.Is(err, tool.ErrSilent):
		return exitError{ExitFailed, fmt.Sprintf(
			"служба есть, но за %s не ответила (%s): она держит реестр тем в памяти так же, как "+
				"говорящая, и запись поверх неё пропала бы; дождись её или %s",
			tool.DialTimeout, err.Error(), portWhenStopped(path))}
	case err != nil:
		return exitError{ExitFailed, "служба идёт, но отвечает отказом: " + err.Error() + "; " + portWhenStopped(path)}
	case resp.Service == nil:
		return exitError{
			ExitFailed,
			"служба идёт, но состояния своего не называет (она старше этой команды); " + portWhenStopped(path),
		}
	}

	return exitError{ExitFailed, fmt.Sprintf(
		"служба идёт с %s: реестр тем она держит в памяти и перепишет файл при первом изменении, "+
			"поэтому положенное поверх неё пропало бы; %s", resp.Service.StartedAt, portWhenStopped(path))}
}

// refuseOwnState отказывает, когда источник переноса лежит в каталоге состояния службы: тогда
// источник и цель — один и тот же файл, и запись нового формата легла бы поверх реестра старого
// моста, стерев сам источник: второй запуск уже не починил бы ничего — новый формат разбирается
// как негодный старый.
func refuseOwnState(cfg *config.Config, registryPath string) error {
	target := filepath.Join(cfg.StateRoot, legacy.TopicsFile)

	if !sameDir(filepath.Dir(registryPath), cfg.StateRoot) {
		return nil
	}

	return exitError{ExitFailed, fmt.Sprintf(
		"реестр старого моста лежит в каталоге состояния службы: источник %s, запись %s — перенос "+
			"записал бы новый формат поверх источника, и реестр старого моста пропал бы; назови другой "+
			"`--from` или другой `state_root` в настройках", registryPath, target)}
}

// sameDir отвечает, один ли это каталог: сравнение идёт по файлу, а не по строке пути, — иначе
// симлинк или /private/var на macOS разводили бы один и тот же каталог.
func sameDir(one, two string) bool {
	first, err := os.Stat(one)
	if err != nil {
		return filepath.Clean(one) == filepath.Clean(two)
	}

	second, err := os.Stat(two)
	if err != nil {
		return filepath.Clean(one) == filepath.Clean(two)
	}

	return os.SameFile(first, second)
}

// portWhenStopped называет порядок: службу снимают, реестр переносят, службу ставят заново.
func portWhenStopped(path string) string {
	return "сними службу (uninstall --config " + path + "), перенеси реестр и поставь её заново " +
		"(install --config " + path + ")"
}

// importAnswerOf переводит исход переноса в машинный ответ; пути источника называет звонящий.
func importAnswerOf(check bool, report legacy.Report) importAnswer {
	skipped := make([]skipAnswer, 0, len(report.Skipped))

	for _, skip := range report.Skipped {
		skipped = append(skipped, skipAnswer{
			Name:   skip.Name,
			ID:     skip.ID,
			Reason: string(skip.Reason),
			Detail: skip.Reason.Text(),
		})
	}

	return importAnswer{
		From: "", Check: check, Read: report.Read, Placed: report.Placed, Skipped: skipped,
		Offset: offsetAnswer{From: "", Old: 0, Service: 0, Outcome: ""},
	}
}

// importLines собирает человеческий отчёт: каждый пропуск — своей строкой с причиной, чтобы
// владелец видел исход одной командой, а не чтением файлов.
func importLines(answer importAnswer) []string {
	lines := make([]string, 0, len(answer.Skipped)+4)
	lines = append(lines, "реестр старого моста: "+answer.From)
	lines = append(lines, "тем прочитано: "+strconv.Itoa(answer.Read))

	if answer.Check {
		lines = append(lines, "не хватает: "+strconv.Itoa(answer.Placed)+" (ничего не записано, ключ `--check`)")
	} else {
		lines = append(lines, "положено: "+strconv.Itoa(answer.Placed))
	}

	lines = append(lines, "пропущено: "+strconv.Itoa(len(answer.Skipped)))

	for _, skip := range answer.Skipped {
		lines = append(lines, fmt.Sprintf("  тема «%s» (id %d): %s", skip.Name, skip.ID, skip.Detail))
	}

	return append(lines, offsetLine(answer.Check, answer.Offset))
}

// offsetLine говорит строкой, что перенос сделал со смещением опроса.
func offsetLine(check bool, offset offsetAnswer) string {
	switch legacy.OffsetOutcome(offset.Outcome) {
	case legacy.OffsetNone:
		return "смещение опроса: в старом состоянии его нет (" + offset.From + ") — переносить нечего"
	case legacy.OffsetKept:
		return fmt.Sprintf("смещение опроса: у службы уже %d, не меньше старого %d — оставлено", offset.Service, offset.Old)
	case legacy.OffsetPlaced:
		if check {
			return fmt.Sprintf("смещение опроса: не хватает %d (ничего не записано, ключ `--check`)", offset.Old)
		}

		return fmt.Sprintf("смещение опроса: %d — перенесено, служба начнёт с него первый опрос", offset.Old)
	default:
		panic("исход переноса смещения вне перечня: " + offset.Outcome)
	}
}

// flagBool читает булев ключ команды; ключа нет — ложь.
func flagBool(cmd *cobra.Command, name string) bool {
	value, err := cmd.Flags().GetBool(name)
	if err != nil {
		return false
	}

	return value
}
