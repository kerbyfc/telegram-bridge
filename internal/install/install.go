// Package install — установка службы моста одной командой и её снятие: бинарник в каталог
// запуска, запись автозапуска ОС, запуск при входе и перезапуск при падении. Каждый шаг
// идемпотентен: уже сделанное не переделывается, а подтверждается строкой отчёта.
//
// Своего файла настроек установка не придумывает и секретов не сочиняет: настройки — дело
// владельца, и без годного `config.yaml` установка не делается вовсе. Состояние службы и её
// журнал снятие не трогает — это данные владельца, а не следы установки.
//
// Служба ветки ставится рядом с рабочей: `--instance <имя>` даёт свою метку автозапуска, своё
// имя бинарника и свой файл настроек, то есть своё состояние, свой адрес границы и своего бота. Без
// этого правку пришлось бы проверять на живом канале владельца.
package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/endpoint"
)

const (
	// BinaryName — имя бинарника рабочей службы в каталоге запуска.
	BinaryName = "telegram-bridge"
	// LabelPrefix — метка службы автозапуска; у службы ветки к ней добавляется имя ветки.
	LabelPrefix = "com.kerbyfc.telegram-bridge"
	// GOOSDarwin и GOOSWindows — ОС, для которых есть автозапуск: запись launchd и задача
	// планировщика.
	GOOSDarwin  = "darwin"
	GOOSWindows = "windows"
	// statuslineRootEnv — переменная окружения, из которой установка берёт корень состояния
	// статусной строки: `CLAUDE_CONFIG_DIR`, тот самый каталог, по которому строку читает Claude
	// Code. Служба её не видит — launchd не наследует окружения оболочки, — поэтому установка
	// называет корень ключом настроек.
	statuslineRootEnv = "CLAUDE_CONFIG_DIR"
)

const (
	dirPerm    os.FileMode = 0o700
	binaryPerm os.FileMode = 0o755
	unitPerm   os.FileMode = 0o644

	// restartWait — сколько установка ждёт ответа перезапущенной службы и её времени старта.
	restartWait = 15 * time.Second
	// restartPoll — пауза между вопросами службе о времени старта.
	restartPoll = 300 * time.Millisecond
)

// instanceRule — чем может быть имя службы ветки: оно идёт в метку автозапуска и в имя файла,
// поэтому своевольных знаков там быть не может.
var instanceRule = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ErrInstall — установка не удалась.
var ErrInstall = errors.New("установка службы моста не удалась")

// Runner запускает команду ОС и отдаёт её вывод: тесты подменяют его и не трогают ни launchd
// настоящей машины, ни её каталог автозапуска.
type Runner func(name string, args ...string) (string, error)

// Options — чем описана установка. Все адреса можно назвать прямо: так установка проверяется на
// своих путях, не трогая ни каталога автозапуска владельца, ни его живой службы.
type Options struct {
	// GOOS — ОС, для которой ставится автозапуск.
	GOOS string
	// Home — домашний каталог; пусто — домашний каталог нынешнего пользователя.
	Home string
	// Instance — имя службы ветки; пусто — рабочая служба.
	Instance string
	// Config — путь к файлу настроек: он уезжает в запись автозапуска переменной
	// `DM_TELEGRAM_CONFIG`, которой служба ветки и отличается от рабочей.
	Config string
	// StateRoot — корень состояния службы: туда ложится вывод её процесса.
	StateRoot string
	// Source — откуда взять бинарник; пусто — свой исполняемый файл.
	Source string
	// Account — SID владельца задачи планировщика Windows; пусто — нынешний пользователь,
	// которого установка спрашивает сама. Тесты называют SID прямо: планировщика на стенде нет.
	Account string
	// BinDir — каталог запуска; пусто — `<home>/.local/bin`.
	BinDir string
	// AgentsDir — каталог записей автозапуска launchd; пусто — `<home>/Library/LaunchAgents`.
	// Планировщику Windows он не нужен: запись живёт в нём самом.
	AgentsDir string
	// Runner — запуск команд ОС; пусто — настоящий запуск.
	Runner Runner
	// Started отвечает, когда поднялся идущий процесс службы, и идёт ли он вообще. Этим
	// установка узнаёт, что в памяти прежняя сборка: файлы на диске о состоянии процесса не
	// говорят ничего. Пусто — установка процесс не спрашивает и решает по файлам.
	Started func() (time.Time, bool)
	// RestartWait — сколько ждать, пока перезапущенная служба ответит и назовёт время старта не
	// раньше нынешнего бинарника; пусто — restartWait. Стенду хватает миллисекунд.
	RestartWait time.Duration
	// Verify сверяет годные настройки с тем, что файлом не проверить: значки тем с их набором в
	// Telegram. Отказ отменяет установку до первого шага, примечание ложится строкой отчёта. Пусто —
	// сверки нет: стенду сеть не нужна.
	Verify func(*config.Config) (string, error)
	// Out — куда печатается отчёт о шагах; пусто — никуда.
	Out io.Writer
}

// Paths — адреса установки.
type Paths struct {
	// Label — метка службы автозапуска.
	Label string
	// Binary — бинарник, который поднимает автозапуск.
	Binary string
	// Unit — запись автозапуска: файл plist у launchd и имя задачи у планировщика Windows, где
	// запись живёт в нём самом.
	Unit string
	// Config — файл настроек, названный записи.
	Config string
	// ServiceLog — вывод самого процесса службы.
	ServiceLog string
}

// Change — итог шага: `Done` — что-то изменилось, `Note` — строка отчёта владельцу.
type Change struct {
	Done bool
	Note string
}

// reporter печатает по строке на шаг: `+` — сделано, `=` — уже так.
type reporter struct {
	out io.Writer
}

// line печатает строку отчёта.
func (r reporter) line(text string) {
	if r.out == nil {
		return
	}

	_, _ = fmt.Fprintln(r.out, text)
}

// step печатает итог шага.
func (r reporter) step(name string, change Change) {
	if r.out == nil || change.Note == "" {
		return
	}

	mark := "="
	if change.Done {
		mark = "+"
	}

	_, _ = fmt.Fprintf(r.out, "  %s %-12s %s\n", mark, name, change.Note)
}

// Install ставит службу: годные настройки, бинарник, запись автозапуска, запуск. Отдаёт адреса
// установки, чтобы звонящий назвал их владельцу.
func Install(opts Options) (Paths, error) {
	paths, service, settings, err := prepare(opts)
	if err != nil {
		return Paths{}, err
	}

	report := reporter{out: opts.Out}
	report.line("ставлю службу моста " + paths.Label)
	report.step("настройки", Change{Done: false, Note: "годны, " + paths.Config})

	if opts.Verify != nil {
		note, verifyErr := opts.Verify(settings)
		if verifyErr != nil {
			return Paths{}, fmt.Errorf("%w: %w", ErrInstall, verifyErr)
		}

		report.step("значки тем", Change{Done: false, Note: note})
	}

	root, err := rootStep(report, paths.Config, settings)
	if err != nil {
		return Paths{}, err
	}

	err = os.MkdirAll(settings.StateRoot, dirPerm)
	if err != nil {
		return Paths{}, fmt.Errorf("%w: каталог состояния %s: %w", ErrInstall, settings.StateRoot, err)
	}

	report.step("состояние", Change{Done: false, Note: settings.StateRoot})

	placed, err := placeBinary(opts.Source, paths.Binary)
	if err != nil {
		return Paths{}, err
	}

	report.step("бинарник", placed)

	wired, err := service.Ensure(paths)
	if err != nil {
		return Paths{}, fmt.Errorf("запись автозапуска: %w", err)
	}

	report.step("автозапуск", wired)

	restart, note := restartNeeded(opts, paths, placed, wired)

	err = service.Start(restart)
	if err != nil {
		return Paths{}, fmt.Errorf("запуск службы: %w", err)
	}

	// Сказать «перезапущена» мало: `/Run` у планировщика при живом экземпляре успешен и ничего не
	// запускает, а launchd отвечает успехом на негодную запись. Поэтому после перезапуска служба
	// спрашивается сама — отвечает ли она и не раньше ли её старт нынешнего бинарника.
	if restart {
		err = confirmRestart(opts, paths.Binary)
		if err != nil {
			return Paths{}, err
		}
	}

	report.step("служба", Change{Done: restart, Note: note})
	ending(report, paths, settings, root)

	return paths, nil
}

// Uninstall снимает автозапуск и бинарник. Состояние, настройки и журнал остаются: это данные
// владельца, а не следы установки, и удалять их за него установка не вправе.
func Uninstall(opts Options) (Paths, error) {
	paths, service, _, err := prepare(opts)
	if err != nil {
		return Paths{}, err
	}

	report := reporter{out: opts.Out}
	report.line("снимаю службу моста " + paths.Label)

	stopped, err := service.Remove(paths)
	if err != nil {
		return Paths{}, fmt.Errorf("снятие автозапуска: %w", err)
	}

	report.step("автозапуск", stopped)

	dropped, err := dropFile(paths.Binary)
	if err != nil {
		return Paths{}, err
	}

	if dropped {
		report.step("бинарник", Change{Done: true, Note: "удалён, " + paths.Binary})
	} else {
		report.step("бинарник", Change{Done: false, Note: "его не было, " + paths.Binary})
	}

	report.line("")
	report.line("состояние и настройки остались: " + paths.Config)

	return paths, nil
}

// Installed отвечает, заведён ли автозапуск этой службы.
func Installed(opts Options) (bool, Paths, error) {
	paths, service, _, err := prepare(opts)
	if err != nil {
		return false, Paths{}, err
	}

	ready, err := service.Installed(paths)
	if err != nil {
		return false, paths, fmt.Errorf("запись автозапуска: %w", err)
	}

	return ready, paths, nil
}

// NewPaths — адреса установки по названным значениям. ОС без автозапуска — отказ, а не установка
// без автозапуска: служба, которую никто не поднимает, не служба.
func NewPaths(opts Options) (Paths, error) {
	label, err := labelOf(opts.Instance)
	if err != nil {
		return Paths{}, err
	}

	home := opts.Home
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("%w: домашний каталог неизвестен: %w", ErrInstall, err)
		}
	}

	if opts.Config == "" {
		return Paths{}, fmt.Errorf("%w: установка без пути к настройкам", ErrInstall)
	}

	cfgPath, err := filepath.Abs(opts.Config)
	if err != nil {
		return Paths{}, fmt.Errorf("%w: путь настроек %s: %w", ErrInstall, opts.Config, err)
	}

	paths := Paths{
		Label:      label,
		Binary:     filepath.Join(binDir(opts, home), binaryOf(opts.Instance)),
		Unit:       "",
		Config:     cfgPath,
		ServiceLog: "",
	}

	switch opts.GOOS {
	case GOOSDarwin:
		paths.Unit = filepath.Join(agentsDir(opts, home), label+".plist")
	case GOOSWindows:
		paths.Binary += ".exe"
		paths.Unit = taskName(paths.Binary)
	default:
		return Paths{}, fmt.Errorf("%w: автозапуска для %s нет", ErrInstall, opts.GOOS)
	}

	return paths, nil
}

// prepare собирает адреса установки, механизм автозапуска и настройки: без годного файла
// настроек установки нет вовсе, а шаги, которым он нужен, читают его здесь, а не заново.
func prepare(opts Options) (Paths, Service, *config.Config, error) {
	paths, err := NewPaths(opts)
	if err != nil {
		return Paths{}, nil, nil, err
	}

	settings, err := config.Load(paths.Config)
	if err != nil {
		return Paths{}, nil, nil, fmt.Errorf("%w: настройки %s: %w", ErrInstall, paths.Config, err)
	}

	state := opts.StateRoot
	if state == "" {
		state = settings.StateRoot
	}

	paths.ServiceLog = filepath.Join(state, ServiceLogName)

	runner := opts.Runner
	if runner == nil {
		runner = ExecRunner
	}

	service, err := PickService(opts.GOOS, runner, paths, opts.Account)
	if err != nil {
		return Paths{}, nil, nil, err
	}

	return paths, service, settings, nil
}

// staleProcess отвечает, идёт ли служба прежней сборкой: её старт раньше правки бинарника,
// который теперь лежит в каталоге запуска. Спросить об этом некого, кроме самой службы: номер
// версии протокола у двух сборок один, а файлы на диске о памяти процесса не знают.
//
// Службы нет, спросить её нечем или бинарник не читается — ответ «нет»: это не «прежняя сборка»,
// а «сборки в памяти нет вовсе», и такую службу поднимет обычный запуск.
func staleProcess(started func() (time.Time, bool), binary string) bool {
	if started == nil {
		return false
	}

	since, running := started()
	if !running {
		return false
	}

	info, err := os.Stat(binary)
	if err != nil {
		return false
	}

	return !buildFresh(since, info.ModTime())
}

// buildFresh отвечает, поднята ли служба не раньше нынешнего бинарника. Точность у двух времён
// разная: время старта службы идёт по протоколу строкой RFC 3339, то есть секундами, а время файла
// — с наносекундами. Сравнивать их как есть нельзя: служба, поднятая в ту же секунду, что записан
// бинарник, выглядела бы старше него — а это штатный первый же `install`, где launchd или
// планировщик успевают за долю секунды. Поэтому время файла усекается до секунды, то есть до той
// точности, которую даёт протокол; поднимать точность протокола (`RFC3339Nano`) значило бы менять
// контракт ради сравнения.
//
// Цена названа: внутри одной секунды «раньше» и «позже» не различаются, и такая пара считается
// свежей. Прежнюю сборку это не пропускает: её старт отстоит на минуты и больше.
func buildFresh(started, modified time.Time) bool {
	return !started.Before(modified.Truncate(time.Second))
}

// restartNeeded отвечает, нужен ли перезапуск службы, и какой строкой о нём отчитаться.
// Перезапуск нужен и тогда, когда сменился только бинарник: запись та же, а код новый, и без
// перезапуска идущая служба осталась бы прежней сборкой. Ничего не менялось — живая служба не
// рвётся.
func restartNeeded(opts Options, paths Paths, placed, wired Change) (bool, string) {
	if wired.Done || placed.Done {
		return true, "идёт"
	}

	// Файлы о состоянии процесса не говорят ничего, поэтому решать по ним одним нельзя:
	// установка, прибитая между заменой бинарника и перезапуском, оставляет новый файл и прежний
	// процесс, а следующий запуск видел бы «всё то же». Старт службы раньше правки её бинарника
	// и значит «в памяти прежняя сборка».
	if staleProcess(opts.Started, paths.Binary) {
		return true, "шла прежней сборкой, перезапущена"
	}

	return false, "идёт"
}

// confirmRestart проверяет, что после перезапуска служба отвечает и поднята нынешней сборкой: её
// старт не раньше правки бинарника. Без этой проверки установка печатает «перезапущена» и тогда,
// когда `/Run` ничего не запустил, а в памяти осталась прежняя сборка.
//
// Спросить нечем (Started пуст) — проверки нет: так установка работает на стенде, где службы нет
// вовсе. Отказ называет, чего именно не хватило: ответа службы или нового времени старта.
func confirmRestart(opts Options, binary string) error {
	if opts.Started == nil {
		return nil
	}

	info, err := os.Stat(binary)
	if err != nil {
		return fmt.Errorf("%w: бинарник %s: %w", ErrInstall, binary, err)
	}

	wait := opts.RestartWait
	if wait == 0 {
		wait = restartWait
	}

	deadline := time.Now().Add(wait)

	for {
		since, running := opts.Started()
		if running && buildFresh(since, info.ModTime()) {
			return nil
		}

		if time.Now().After(deadline) {
			if !running {
				return fmt.Errorf(
					"%w: служба не ответила за %s после перезапуска — поднялась ли она и та ли это сборка, неизвестно",
					ErrInstall, wait,
				)
			}

			return fmt.Errorf(
				"%w: служба идёт с %s, а бинарник новее (%s): перезапуск не подхватил новую сборку",
				ErrInstall, since.Truncate(time.Second).Format(time.RFC3339),
				info.ModTime().Truncate(time.Second).Format(time.RFC3339),
			)
		}

		time.Sleep(restartPoll)
	}
}

// ending печатает, что владельцу делать дальше.
func ending(report reporter, paths Paths, settings *config.Config, statuslineRoot string) {
	report.line("")
	report.line("настройки       " + paths.Config)
	report.line("состояние       " + settings.StateRoot)
	report.line(fmt.Sprintf("%-16s%s", endpoint.Word(), settings.Endpoint()))
	report.line("журнал          " + settings.Log())
	report.line("вывод процесса  " + paths.ServiceLog)
	report.line("корень состояния: " + statuslineRoot)
	report.line("")
	report.line("самопроверка:   telegram-bridge doctor --config " + paths.Config)
	report.line("снять:          telegram-bridge uninstall --config " + paths.Config)
}

// rootStep называет корень состояния статусной строки и печатает строку отчёта об этом шаге.
func rootStep(report reporter, path string, settings *config.Config) (string, error) {
	root, change, err := nameStatuslineRoot(path, settings)
	if err != nil {
		return "", err
	}

	report.step("корень", change)

	return root, nil
}

// nameStatuslineRoot называет корень состояния статусной строки ключом настроек, когда владелец
// его не назвал: корень берётся из окружения установки, а без него — `~/.claude`. Уже названное
// значение не трогается. Отдаёт корень, который увидит служба, и строку отчёта об этом шаге.
func nameStatuslineRoot(path string, settings *config.Config) (string, Change, error) {
	if settings.StatuslineNamed() {
		root := settings.Statusline()

		return root, Change{Done: false, Note: "уже назван, " + root}, nil
	}

	root, err := statuslineRootValue()
	if err != nil {
		return "", Change{}, err
	}

	err = addSettingsKey(path, config.StatuslineRootKey, root.written)
	if err != nil {
		return "", Change{}, err
	}

	return root.expanded, Change{Done: true, Note: "назван, " + root.expanded}, nil
}

// statuslineRoot — что установка пишет в ключ и каким корень увидит служба.
type statuslineRoot struct {
	// written — значение ключа как оно ляжет в файл: корень из окружения или `~/.claude`.
	written string
	// expanded — тот же корень, раскрытый для отчёта и проверки.
	expanded string
}

// statuslineRootValue проверяет корень, который установка видит в своём окружении, до правки файла:
// негодный путь — отказ, а не ключ, оставленный в настройках владельца. Без переменной корень —
// `~/.claude`; тильда законна, её раскрывает при загрузке служба, у которой рабочий каталог не
// домашний, а на Windows — `~\`. Относительный корень — отказ: от чьего каталога его считать,
// установка не знает, а служба под автозапуском стоит не там, где шла установка.
func statuslineRootValue() (statuslineRoot, error) {
	written := os.Getenv(statuslineRootEnv)
	if written == "" {
		written = filepath.Join("~", ".claude")
	}

	expanded, err := config.ExpandHome(written)
	if err != nil {
		return statuslineRoot{}, fmt.Errorf("%w: корень состояния: %w", ErrInstall, err)
	}

	if !filepath.IsAbs(expanded) {
		return statuslineRoot{}, fmt.Errorf(
			"%w: корень состояния %q не абсолютный: назови полный путь в `%s`",
			ErrInstall, written, statuslineRootEnv,
		)
	}

	return statuslineRoot{written: written, expanded: expanded}, nil
}

// addSettingsKey дописывает ключ в файл настроек, сохраняя остальное содержимое: правка идёт через
// временный файл и переименование, поэтому полупустых настроек службе не достаётся. Значение —
// одна строка YAML в одинарных кавычках: путь с пробелом или обратной косой чертой (Windows)
// остаётся собой, а не становится управляющей последовательностью.
func addSettingsKey(path, key, value string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: настройки %s: %w", ErrInstall, path, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: настройки %s: %w", ErrInstall, path, err)
	}

	if len(body) > 0 && body[len(body)-1] != '\n' {
		body = append(body, '\n')
	}

	body = append(body, key+": '"+strings.ReplaceAll(value, "'", "''")+"'\n"...)

	err = writeFile(path, body, info.Mode().Perm())
	if err != nil {
		return err
	}

	return nil
}

// labelOf — метка службы: рабочая или ветки.
func labelOf(instance string) (string, error) {
	if instance == "" {
		return LabelPrefix, nil
	}

	if !instanceRule.MatchString(instance) {
		return "", fmt.Errorf(
			"%w: имя службы ветки %q негодно: строчные буквы, цифры и дефис, до 32 знаков",
			ErrInstall, instance,
		)
	}

	return LabelPrefix + "." + instance, nil
}

// binaryOf — имя бинарника: у службы ветки своё, чтобы её бинарник не вытеснял рабочий.
func binaryOf(instance string) string {
	if instance == "" {
		return BinaryName
	}

	return BinaryName + "-" + instance
}

// taskName — имя задачи планировщика Windows: то же, что имя бинарника службы без `.exe`, поэтому
// у службы ветки своя задача, и две службы друг друга не подменяют.
func taskName(binary string) string {
	return strings.TrimSuffix(filepath.Base(binary), ".exe")
}

// binDir — каталог запуска.
func binDir(opts Options, home string) string {
	if opts.BinDir != "" {
		return opts.BinDir
	}

	return filepath.Join(home, ".local", "bin")
}

// agentsDir — каталог записей автозапуска.
func agentsDir(opts Options, home string) string {
	if opts.AgentsDir != "" {
		return opts.AgentsDir
	}

	return filepath.Join(home, "Library", "LaunchAgents")
}
