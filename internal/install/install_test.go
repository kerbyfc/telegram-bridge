package install_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/install"
)

// launchctl — команда автозапуска macOS: имя названо один раз, им же сверяются вызовы стенда.
const launchctl = "launchctl"

// recorder — поддельный запуск команд ОС: помнит вызовы и отвечает тем, чем велено. Настоящий
// launchd, настоящий каталог автозапуска и живая служба в тестах не участвуют вовсе.
type recorder struct {
	mu         sync.Mutex
	calls      []string
	registered bool
}

// run — запуск команды: `launchctl print` отвечает по тому, зарегистрирована ли служба, а
// `bootstrap` её регистрирует.
func (r *recorder) run(name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	call := strings.TrimSpace(name + " " + strings.Join(args, " "))
	r.calls = append(r.calls, call)

	if name != launchctl || len(args) == 0 {
		return "", nil
	}

	switch args[0] {
	case "print":
		if !r.registered {
			return "", os.ErrNotExist
		}
	case "bootstrap":
		r.registered = true
	case "bootout":
		r.registered = false
	}

	return "", nil
}

// saw отвечает, был ли вызов, начинающийся с названного текста.
func (r *recorder) saw(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, call := range r.calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}

	return false
}

// TestInstallWritesUnitAndStartsService: установка кладёт бинарник, пишет запись автозапуска с
// путём к настройкам и поднимает службу.
func TestInstallWritesUnitAndStartsService(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}

	paths, err := install.Install(options(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	if paths.Label != install.LabelPrefix {
		t.Fatalf("метка рабочей службы не та: %q", paths.Label)
	}

	if paths.Binary != filepath.Join(home, ".local", "bin", install.BinaryName) {
		t.Fatalf("бинарник встал не в каталог запуска: %q", paths.Binary)
	}

	info, err := os.Stat(paths.Binary)
	if err != nil {
		t.Fatalf("бинарника нет: %v", err)
	}

	if info.Mode().Perm() != 0o755 {
		t.Fatalf("бинарник не исполняемый: %04o", info.Mode().Perm())
	}

	checkUnit(t, paths, cfgPath)

	if !runs.saw("plutil -lint") {
		t.Fatal("запись не проверена до launchctl: негодный plist он отвергает тем же отказом, что гонку")
	}

	if !runs.saw("launchctl bootstrap") || !runs.saw("launchctl kickstart") {
		t.Fatalf("служба не зарегистрирована и не поднята: %v", runs.calls)
	}
}

// TestSecondInstallKeepsLiveServiceRunning: повторная установка без правок ничего не переделывает
// и живую службу не рвёт — `kickstart` без `-k` идущий процесс не трогает.
func TestSecondInstallKeepsLiveServiceRunning(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}

	_, err := install.Install(options(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	again := &recorder{mu: sync.Mutex{}, calls: nil, registered: true}

	_, err = install.Install(options(home, cfgPath, source, "", again))
	if err != nil {
		t.Fatalf("повторная установка не прошла: %v", err)
	}

	if again.saw("launchctl bootout") || again.saw("launchctl bootstrap") {
		t.Fatalf("повторная установка переделала запись: %v", again.calls)
	}

	if again.saw("launchctl kickstart -k") {
		t.Fatalf("повторная установка перезапустила живую службу: %v", again.calls)
	}
}

// TestChangedBinaryRestartsService: новый бинарник подхватывается только перезапуском, поэтому
// сменившаяся установка поднимает службу заново.
func TestChangedBinaryRestartsService(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}

	_, err := install.Install(options(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	err = os.WriteFile(source, []byte("#!/bin/sh\necho новая сборка\n"), 0o755)
	if err != nil {
		t.Fatalf("новая сборка не записана: %v", err)
	}

	// Запись при этом не менялась вовсе: перезапуск нужен из-за нового бинарника, иначе идущая
	// служба осталась бы прежней сборкой.
	again := &recorder{mu: sync.Mutex{}, calls: nil, registered: true}

	_, err = install.Install(options(home, cfgPath, source, "", again))
	if err != nil {
		t.Fatalf("повторная установка не прошла: %v", err)
	}

	if !again.saw("launchctl kickstart -k") {
		t.Fatalf("служба не перезапущена под новый бинарник: %v", again.calls)
	}
}

// TestReinstallRestoresRemovedBinary: бинарник, снесённый руками мимо снятия, служба переживает —
// процесс уже в памяти, — и звонящие его не находят. Повторная установка это чинит сама: кладёт
// бинарник на место и перезапускает службу, а не отвечает «всё то же» по нетронутой записи.
func TestReinstallRestoresRemovedBinary(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}

	paths, err := install.Install(options(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	err = os.Remove(paths.Binary)
	if err != nil {
		t.Fatalf("бинарник не снесён: %v", err)
	}

	again := &recorder{mu: sync.Mutex{}, calls: nil, registered: true}

	_, err = install.Install(options(home, cfgPath, source, "", again))
	if err != nil {
		t.Fatalf("повторная установка не прошла: %v", err)
	}

	_, err = os.Stat(paths.Binary)
	if err != nil {
		t.Fatalf("повторная установка не вернула бинарник: %v", err)
	}

	if !again.saw("launchctl kickstart -k") {
		t.Fatalf("служба без бинарника не перезапущена: %v", again.calls)
	}
}

// TestBranchServiceStandsBesideWorking: служба ветки ставится рядом с рабочей — своя метка, своё
// имя бинарника, своя запись. Иначе правку пришлось бы проверять на живом канале владельца.
func TestBranchServiceStandsBesideWorking(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}

	working, err := install.Install(options(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка рабочей службы не прошла: %v", err)
	}

	branch, err := install.Install(options(home, cfgPath, source, "06-sluzhba", runs))
	if err != nil {
		t.Fatalf("установка службы ветки не прошла: %v", err)
	}

	if branch.Label == working.Label || branch.Binary == working.Binary || branch.Unit == working.Unit {
		t.Fatalf("служба ветки встала поверх рабочей: %+v против %+v", branch, working)
	}

	if !strings.HasSuffix(branch.Label, ".06-sluzhba") {
		t.Fatalf("метка службы ветки не несёт её имени: %q", branch.Label)
	}

	_, err = os.Stat(working.Unit)
	if err != nil {
		t.Fatalf("запись рабочей службы пропала: %v", err)
	}
}

// TestUninstallKeepsOwnerData: снятие убирает автозапуск и бинарник, но состояние и настройки
// оставляет: это данные владельца, а не следы установки.
func TestUninstallKeepsOwnerData(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}

	paths, err := install.Install(options(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	_, err = install.Uninstall(options(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("снятие не прошло: %v", err)
	}

	_, err = os.Stat(paths.Unit)
	if !os.IsNotExist(err) {
		t.Fatalf("запись автозапуска осталась: %v", err)
	}

	_, err = os.Stat(paths.Binary)
	if !os.IsNotExist(err) {
		t.Fatalf("бинарник остался: %v", err)
	}

	_, err = os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("снятие унесло настройки владельца: %v", err)
	}

	ready, _, err := install.Installed(options(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("проверка установки отказала: %v", err)
	}

	if ready {
		t.Fatal("снятая служба числится установленной")
	}
}

// TestInstallRefusesWithoutSettings: без годных настроек установки нет вовсе — своего файла
// настроек она не придумывает и секретов не сочиняет.
func TestInstallRefusesWithoutSettings(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}

	err := os.Remove(cfgPath)
	if err != nil {
		t.Fatalf("настройки не убраны: %v", err)
	}

	_, err = install.Install(options(home, cfgPath, source, "", runs))
	if err == nil {
		t.Fatal("установка прошла без настроек")
	}
}

// TestPathsRefuseUnknownAutostart: ОС без автозапуска — отказ, а не установка без автозапуска:
// служба, которую никто не поднимает, не служба.
func TestPathsRefuseUnknownAutostart(t *testing.T) {
	t.Parallel()

	home, cfgPath, _ := stand(t)

	_, err := install.NewPaths(install.Options{ //nolint:exhaustruct // адресам хватает ОС, дома и настроек
		GOOS: "plan9", Home: home, Config: cfgPath,
	})
	if err == nil {
		t.Fatal("адреса установки собрались для ОС без автозапуска")
	}
}

// TestPathsForWindows: у установки Windows свои адреса — бинарник с `.exe` и задача планировщика в
// роли записи автозапуска.
func TestPathsForWindows(t *testing.T) {
	t.Parallel()

	home, cfgPath, _ := stand(t)

	paths, err := install.NewPaths(install.Options{ //nolint:exhaustruct // адресам хватает ОС, дома и настроек
		GOOS: install.GOOSWindows, Home: home, Config: cfgPath,
	})
	if err != nil {
		t.Fatalf("адреса установки Windows не собраны: %v", err)
	}

	if paths.Binary != filepath.Join(home, ".local", "bin", install.BinaryName+".exe") {
		t.Fatalf("бинарник Windows не назван: %q", paths.Binary)
	}

	if paths.Unit != install.BinaryName {
		t.Fatalf("задача планировщика названа не по службе: %q", paths.Unit)
	}
}

// TestBadInstanceNameRefused: имя службы ветки идёт в метку автозапуска и в имя файла, поэтому
// своевольных знаков в нём быть не может.
func TestBadInstanceNameRefused(t *testing.T) {
	t.Parallel()

	home, cfgPath, _ := stand(t)

	for _, name := range []string{"Ветка", "../уход", "с пробелом", strings.Repeat("a", 33)} {
		_, err := install.NewPaths(install.Options{ //nolint:exhaustruct // адресам хватает ОС, дома, имени и настроек
			GOOS: install.GOOSDarwin, Home: home, Instance: name, Config: cfgPath,
		})
		if err == nil {
			t.Fatalf("имя службы ветки %q принято", name)
		}
	}
}

// checkUnit сверяет запись автозапуска: метка, бинарник с подкомандой службы, путь к настройкам
// переменной окружения и файл вывода процесса.
func checkUnit(t *testing.T, paths install.Paths, cfgPath string) {
	t.Helper()

	unit, err := os.ReadFile(paths.Unit)
	if err != nil {
		t.Fatalf("записи автозапуска нет: %v", err)
	}

	want := []string{
		install.LabelPrefix, paths.Binary, "serve",
		"DM_TELEGRAM_CONFIG", cfgPath, install.ServiceLogName,
	}

	for _, part := range want {
		if !strings.Contains(string(unit), part) {
			t.Fatalf("в записи автозапуска нет %q:\n%s", part, unit)
		}
	}
}

// TestStaleProcessRestartsService: прерванная установка оставляет новый бинарник на диске и
// прежний процесс в памяти, а файлы об этом не говорят ничего. Повторная установка спрашивает саму
// службу: её старт раньше правки бинарника — значит в памяти прежняя сборка, и нужен перезапуск.
func TestStaleProcessRestartsService(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}

	paths, err := install.Install(options(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	info, err := os.Stat(paths.Binary)
	if err != nil {
		t.Fatalf("бинарника нет: %v", err)
	}

	again := &recorder{mu: sync.Mutex{}, calls: nil, registered: true}
	stale := options(home, cfgPath, source, "", again)

	// Первый вопрос — тот, по которому установка решает, что в памяти прежняя сборка; второй и
	// следующие — подтверждение перезапуска, и после него служба отвечает нынешним временем старта.
	// Время старта приходит по протоколу секундами: стенд отдаёт то же, что `serviceStarted` после
	// разбора `started_at`, — иначе сравнение в тесте точнее, чем в жизни.
	asked := 0
	stale.Started = func() (time.Time, bool) {
		asked++
		if asked == 1 {
			return info.ModTime().Add(-time.Minute).Truncate(time.Second), true
		}

		return info.ModTime().Truncate(time.Second), true
	}

	_, err = install.Install(stale)
	if err != nil {
		t.Fatalf("повторная установка не прошла: %v", err)
	}

	if !again.saw("launchctl kickstart -k") {
		t.Fatalf("служба осталась прежней сборкой: %v", again.calls)
	}

	// Служба, поднятая после правки бинарника, идёт нынешней сборкой — рвать её незачем.
	fresh := &recorder{mu: sync.Mutex{}, calls: nil, registered: true}
	live := options(home, cfgPath, source, "", fresh)
	live.Started = func() (time.Time, bool) { return info.ModTime().Add(time.Minute), true }

	_, err = install.Install(live)
	if err != nil {
		t.Fatalf("третья установка не прошла: %v", err)
	}

	if fresh.saw("launchctl kickstart -k") {
		t.Fatalf("живая служба нынешней сборки перезапущена без причины: %v", fresh.calls)
	}
}

// TestInstallKeepsOwnerStatuslineRoot: корень, названный владельцем ключом, установка не затирает
// и называет его в отчёте — тот, который увидит служба под автозапуском.
func TestInstallKeepsOwnerStatuslineRoot(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)

	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}
	owner := options(home, cfgPath, source, "", runs)

	var report bytes.Buffer

	owner.Out = &report

	_, err = install.Install(owner)
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	root := filepath.Join(home, "statusline")

	if !strings.Contains(report.String(), "корень состояния: "+root) {
		t.Fatalf("отчёт не назвал корень состояния: %s\n%s", root, report.String())
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("настройки не прочитаны после установки: %v", err)
	}

	if !bytes.Equal(before, after) {
		t.Fatalf("названные владельцем настройки переписаны:\n%s", after)
	}
}

// TestInstallKeepsStatuslineRootCase: имя ключа без учёта регистра — viper его не различает, поэтому
// `STATUSLINE_ROOT` владельца остаётся его ключом: установка не дописывает строчный и не перекрывает
// названное значение.
func TestInstallKeepsStatuslineRootCase(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	root := filepath.Join(home, "верхний-регистр")
	cfgPath = withStatuslineRootKey(t, cfgPath, "STATUSLINE_ROOT", root)

	before := read(t, cfgPath)

	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}
	owner := options(home, cfgPath, source, "", runs)

	var report bytes.Buffer

	owner.Out = &report

	_, err := install.Install(owner)
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	if !bytes.Equal(before, read(t, cfgPath)) {
		t.Fatalf("ключ владельца в другом регистре переписан:\n%s", read(t, cfgPath))
	}

	if !strings.Contains(report.String(), "корень состояния: "+root) {
		t.Fatalf("отчёт не назвал корень из ключа в другом регистре:\n%s", report.String())
	}
}

// TestInstallNamesStatuslineRootFromEnv: ключа нет — установка пишет корень, который видит в своём
// окружении, то есть `CLAUDE_CONFIG_DIR`: до службы под автозапуском переменная не доходит.
func TestInstallNamesStatuslineRootFromEnv(t *testing.T) {
	root := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", root)

	home, cfgPath, source := stand(t)
	cfgPath = withoutStatuslineRoot(t, cfgPath)

	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}
	named := options(home, cfgPath, source, "", runs)

	var report bytes.Buffer

	named.Out = &report

	_, err := install.Install(named)
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	if !strings.Contains(string(read(t, cfgPath)), "statusline_root: '"+root+"'") {
		t.Fatalf("корень окружения не записан ключом:\n%s", read(t, cfgPath))
	}

	if !strings.Contains(report.String(), "корень состояния: "+root) {
		t.Fatalf("отчёт не назвал корень окружения: %s\n%s", root, report.String())
	}
}

// TestInstallNamesStatuslineRootDefault: без `CLAUDE_CONFIG_DIR` установка пишет `~/.claude` — тем
// же ключом, а тильду служба раскроет при загрузке настроек.
func TestInstallNamesStatuslineRootDefault(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	home, cfgPath, source := stand(t)
	cfgPath = withoutStatuslineRoot(t, cfgPath)

	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}
	unnamed := options(home, cfgPath, source, "", runs)

	var report bytes.Buffer

	unnamed.Out = &report

	_, err := install.Install(unnamed)
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	written := filepath.Join("~", ".claude")

	if !strings.Contains(string(read(t, cfgPath)), "statusline_root: '"+written+"'") {
		t.Fatalf("корень по умолчанию не записан ключом:\n%s", read(t, cfgPath))
	}

	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("домашний каталог неизвестен: %v", err)
	}

	if !strings.Contains(report.String(), "корень состояния: "+filepath.Join(userHome, ".claude")) {
		t.Fatalf("отчёт не назвал раскрытый корень по умолчанию:\n%s", report.String())
	}
}

// TestInstallRefusesEmptyStatuslineRoot: ключ, названный без пути, — отказ загрузки настроек, и
// установка файл владельца не трогает: второго такого ключа в нём не появляется ни для пустого
// значения, ни для `~` (null по YAML).
func TestInstallRefusesEmptyStatuslineRoot(t *testing.T) {
	t.Parallel()

	for _, written := range []struct {
		name  string
		value string
	}{
		{name: "без значения", value: ""},
		{name: "null", value: "~"},
	} {
		home, cfgPath, source := stand(t)
		cfgPath = withStatuslineRoot(t, cfgPath, written.value)

		before := read(t, cfgPath)

		runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}
		refused := options(home, cfgPath, source, "", runs)

		_, err := install.Install(refused)
		if err == nil {
			t.Fatalf("%s: установка прошла с корнем без пути", written.name)
		}

		if !bytes.Equal(before, read(t, cfgPath)) {
			t.Fatalf("%s: файл настроек тронут при отказе:\n%s", written.name, read(t, cfgPath))
		}

		if !strings.Contains(err.Error(), "убери ключ") {
			t.Fatalf("%s: отказ не говорит, что делать: %v", written.name, err)
		}
	}
}

// TestInstallRefusesBadStatuslineRootEnv: корень из окружения проверяется до правки файла: `~user` и
// относительный путь — отказ, и ключ в настройки владельца не попадает.
//
//nolint:paralleltest // корень берётся из окружения: t.Setenv запрещает параллельный тест
func TestInstallRefusesBadStatuslineRootEnv(t *testing.T) {
	for _, written := range []struct {
		name  string
		value string
	}{
		{name: "чужой домашний каталог", value: "~someone/корень"},
		{name: "относительный путь", value: "корень"},
	} {
		t.Setenv("CLAUDE_CONFIG_DIR", written.value)

		home, cfgPath, source := stand(t)
		cfgPath = withoutStatuslineRoot(t, cfgPath)

		before := read(t, cfgPath)

		runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}
		bad := options(home, cfgPath, source, "", runs)

		_, err := install.Install(bad)
		if err == nil {
			t.Fatalf("%s: установка приняла негодный корень", written.name)
		}

		if !strings.Contains(err.Error(), "корень состояния") {
			t.Fatalf("%s: отказ не назвал корень: %v", written.name, err)
		}

		if !bytes.Equal(before, read(t, cfgPath)) {
			t.Fatalf("%s: файл настроек тронут до проверки корня:\n%s", written.name, read(t, cfgPath))
		}
	}
}

// withoutStatuslineRoot пишет копию настроек стенда без ключа корня состояния и отдаёт её путь.
func withoutStatuslineRoot(t *testing.T, cfgPath string) string {
	t.Helper()

	kept := make([]string, 0, 8)

	for _, line := range strings.Split(string(read(t, cfgPath)), "\n") {
		if !strings.HasPrefix(line, "statusline_root:") {
			kept = append(kept, line)
		}
	}

	path := filepath.Join(filepath.Dir(cfgPath), "config-no-root.yaml")

	err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o600)
	if err != nil {
		t.Fatalf("настройки без ключа корня не записаны: %v", err)
	}

	return path
}

// withStatuslineRoot пишет копию настроек стенда с ключом корня, названным значением value, и
// отдаёт её путь.
func withStatuslineRoot(t *testing.T, cfgPath, value string) string {
	t.Helper()

	return withStatuslineRootKey(t, cfgPath, "statusline_root", value)
}

// withStatuslineRootKey пишет копию настроек стенда с ключом корня, переименованным в key, и
// отдаёт её путь.
func withStatuslineRootKey(t *testing.T, cfgPath, key, value string) string {
	t.Helper()

	kept := make([]string, 0, 8)

	for _, line := range strings.Split(string(read(t, cfgPath)), "\n") {
		if strings.HasPrefix(line, "statusline_root:") {
			line = key + ": " + value
		}

		kept = append(kept, line)
	}

	path := filepath.Join(filepath.Dir(cfgPath), "config-named.yaml")

	err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o600)
	if err != nil {
		t.Fatalf("настройки с ключом корня не записаны: %v", err)
	}

	return path
}

// read читает файл стенда.
func read(t *testing.T, path string) []byte {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s не прочитан: %v", path, err)
	}

	return body
}

// TestUnitPassesPlutil: запись автозапуска проверяется настоящим `plutil -lint` на файле стенда.
// launchd отвергает негодный plist тем же Input/output error, что и гонку домена, поэтому
// опечатка в шаблоне выглядела бы как случайная неудача установки, а не как её причина.
//
// Настоящей службы здесь нет: `plutil` только читает названный файл в своём временном каталоге.
func TestUnitPassesPlutil(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != install.GOOSDarwin {
		t.Skip("plutil есть только на macOS")
	}

	home, cfgPath, source := stand(t)
	runs := &recorder{mu: sync.Mutex{}, calls: nil, registered: false}

	paths, err := install.Install(options(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	out, err := exec.Command("plutil", "-lint", paths.Unit).CombinedOutput() //nolint:gosec,noctx // свой файл стенда
	if err != nil {
		t.Fatalf("plutil отверг запись автозапуска: %v: %s", err, out)
	}
}

// stand готовит стенд установки: свой дом, свой файл настроек, своя «сборка» бинарника. Ни один
// адрес стенда не ведёт в систему владельца.
func stand(t *testing.T) (string, string, string) {
	t.Helper()

	home := t.TempDir()
	state := filepath.Join(home, "state")
	cfgPath := filepath.Join(home, "config.yaml")

	err := os.MkdirAll(state, 0o700)
	if err != nil {
		t.Fatalf("каталог состояния не создан: %v", err)
	}

	settings := strings.Join([]string{
		"bot_token: \"1234567:секрет\"",
		"group_id: -1001234567890",
		"owner_id: 11111111",
		"state_root: " + state,
		"default_topic_name: по-умолчанию",
		"statusline_root: " + filepath.Join(home, "statusline"),
		"",
	}, "\n")

	err = os.WriteFile(cfgPath, []byte(settings), 0o600)
	if err != nil {
		t.Fatalf("настройки не записаны: %v", err)
	}

	source := filepath.Join(home, "build", "telegram-bridge")

	err = os.MkdirAll(filepath.Dir(source), 0o700)
	if err != nil {
		t.Fatalf("каталог сборки не создан: %v", err)
	}

	err = os.WriteFile(source, []byte("#!/bin/sh\necho сборка\n"), 0o755)
	if err != nil {
		t.Fatalf("сборка не записана: %v", err)
	}

	return home, cfgPath, source
}

// options собирает описание установки на путях стенда.
func options(home, cfgPath, source, instance string, runs *recorder) install.Options {
	return install.Options{
		GOOS:        install.GOOSDarwin,
		Home:        home,
		Instance:    instance,
		Config:      cfgPath,
		StateRoot:   "",
		Source:      source,
		Account:     "",
		BinDir:      "",
		AgentsDir:   "",
		Runner:      runs.run,
		Started:     nil,
		RestartWait: 0,
		Out:         nil,
	}
}
