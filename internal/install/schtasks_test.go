package install_test

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/kerbyfc/telegram-bridge/internal/install"
)

// ownerSID — SID владельца задачи на стенде: настоящий планировщик Windows здесь не участвует, а
// запись требует адреса владельца.
const ownerSID = "S-1-5-21-1004336348-1177238915-682003330-1001"

const (
	// schtasksCommand — имя команды планировщика: им же сверяются вызовы стенда.
	schtasksCommand = "schtasks"
	// taskName — имя задачи стенда: то же, что у рабочей службы.
	taskName = "telegram-bridge"
	// image — имя процесса службы стенда.
	image = "telegram-bridge.exe"
)

// scheduler — поддельный планировщик задач Windows: помнит вызовы, держит XML задачи и её процесс.
// Ни настоящей задачи, ни настоящего запуска в тестах нет.
type scheduler struct {
	mu      sync.Mutex
	calls   []string
	task    string
	created []byte
	running bool
	// listFails — список задач спросить нельзя: так планировщик отвечает отказом доступа.
	listFails bool
	// xmlFails — список отдаётся, а запись задачи не читается.
	xmlFails bool
	// singleByte — запись задачи приходит однобайтной кодировкой консоли, а не UTF-16 с BOM.
	singleByte bool
	// garbage — запись задачи приходит не XML: такой вывод сверять нечем.
	garbage bool
	// hiddenFromList — задача не попадает в общий список: так было бы, если бы список не отдавал
	// скрытые задачи.
	hiddenFromList bool
	// lingering — сколько ответов `tasklist` процесс службы ещё виден после остановки задачи:
	// столько установка и ждёт.
	lingering int
}

// run — запуск команды ОС: `schtasks` и `tasklist` отвечают по состоянию стенда. Ветка на вызов
// планировщика — не сложность, а стенд.
//
//nolint:cyclop,funlen // стенд: одна ветка на каждый вызов планировщика
func (s *scheduler) run(name string, args ...string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))

	switch {
	case name == schtasksCommand && hasArg(args, "/Query") && hasArg(args, "/XML"):
		if s.task == "" || s.xmlFails {
			return "", os.ErrNotExist
		}

		if s.garbage {
			return "не xml вовсе", nil
		}

		if s.singleByte {
			return decodeTask([]byte(s.task)), nil
		}

		return s.task, nil
	case name == schtasksCommand && hasArg(args, "/Query") && hasArg(args, "/TN"):
		if s.task == "" {
			return "", os.ErrNotExist
		}

		return "задача есть", nil
	case name == schtasksCommand && hasArg(args, "/Query"):
		if s.listFails {
			return "", os.ErrPermission
		}

		if s.task == "" || s.hiddenFromList {
			return "", nil
		}

		return fmt.Sprintf("\"\\%s\",\"N/A\",\"Ready\"\n", taskName), nil
	case name == schtasksCommand && hasArg(args, "/Create"):
		file := argAfter(args, "/XML")

		body, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("файл задачи не прочитан: %w", err)
		}

		s.created = body
		s.task = string(body)

		return "", nil
	case name == schtasksCommand && hasArg(args, "/Run"):
		s.running = true

		return "", nil
	case name == schtasksCommand && hasArg(args, "/End"):
		s.running = false

		return "", nil
	case name == schtasksCommand && hasArg(args, "/Delete"):
		s.task = ""

		return "", nil
	}

	// tasklist: процесс службы идёт, пока задача запущена, и ещё столько ответов, сколько
	// велено ждать после её остановки.
	if name == "tasklist" {
		if s.lingering > 0 {
			s.lingering--

			return fmt.Sprintf("\"%s\",\"1234\",\"Console\",\"1\",\"1 000 КБ\"\n", image), nil
		}

		if s.running {
			return fmt.Sprintf("\"%s\",\"1234\",\"Console\",\"1\",\"1 000 КБ\"\n", image), nil
		}

		return "", nil
	}

	return "", nil
}

// saw отвечает, был ли вызов, начинающийся с названного текста.
func (s *scheduler) saw(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, call := range s.calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}

	return false
}

// forget стирает память стенда между установками: сверяется только то, что сделала вторая.
func (s *scheduler) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = nil
	s.created = nil
}

// TestWindowsInstallCreatesScheduledTask: установка кладёт задачу планировщика с триггером входа
// владельца, перезапуском при падении и подкомандой службы, и поднимает её.
func TestWindowsInstallCreatesScheduledTask(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	paths, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	if paths.Unit != "telegram-bridge" {
		t.Fatalf("задача названа не по службе: %q", paths.Unit)
	}

	if paths.Binary != filepath.Join(home, ".local", "bin", "telegram-bridge.exe") {
		t.Fatalf("бинарник встал не в каталог запуска: %q", paths.Binary)
	}

	unit := taskText(t, runs.created)

	for _, want := range []string{
		"<LogonTrigger>", "<UserId>" + ownerSID + "</UserId>", "<Hidden>true</Hidden>",
		"<RestartOnFailure>", "<Command>" + paths.Binary + "</Command>",
		"serve --service --config " + cfgPath,
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("в задаче нет %q:\n%s", want, unit)
		}
	}

	if !runs.saw("schtasks /Create") || !runs.saw("schtasks /Run") {
		t.Fatalf("задача не создана и служба не поднята: %v", runs.calls)
	}

	// Только что заведённая задача не идёт: останавливать нечего, и `/End` на ней тоже отказ.
	if runs.saw("schtasks /End") {
		t.Fatalf("установка останавливала не запущенную задачу: %v", runs.calls)
	}
}

// TestWindowsTaskIsWellFormedXML: запись задачи разбирается как XML. Планировщик отвергает файл
// целиком, если в комментарии встретится `--`, и установка падает на создании задачи.
func TestWindowsTaskIsWellFormedXML(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	decoder := xml.NewDecoder(strings.NewReader(decodeTask(runs.created)))
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	for {
		_, err = decoder.Token()
		if errors.Is(err, io.EOF) {
			return
		}

		if err != nil {
			t.Fatalf("запись задачи не разбирается как XML: %v", err)
		}
	}
}

// TestWindowsSecondInstallKeepsTask: повторная установка без правок задачу не переписывает и живую
// службу не рвёт — «уже так» подтверждается строкой отчёта.
func TestWindowsSecondInstallKeepsTask(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	runs.forget()

	var report bytes.Buffer

	again := windowsOptions(home, cfgPath, source, "", runs)
	again.Out = &report

	_, err = install.Install(again)
	if err != nil {
		t.Fatalf("повторная установка не прошла: %v", err)
	}

	if runs.saw("schtasks /Create") {
		t.Fatalf("повторная установка переписала задачу: %v", runs.calls)
	}

	if runs.saw("schtasks /End") {
		t.Fatalf("повторная установка перезапустила живую службу: %v", runs.calls)
	}

	if !strings.Contains(report.String(), "уже настроен, задача telegram-bridge") {
		t.Fatalf("отчёт не подтвердил, что задача уже стоит:\n%s", report.String())
	}
}

// TestWindowsChangedSettingsRewriteTask: разошлась запись — задача переписывается. Отпечаток несёт
// путь к настройкам, поэтому служба ветки со своим файлом — другая задача, а не та же.
func TestWindowsChangedSettingsRewriteTask(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	other := filepath.Join(home, "config-ветки.yaml")

	err = os.WriteFile(other, read(t, cfgPath), 0o600)
	if err != nil {
		t.Fatalf("второй файл настроек не записан: %v", err)
	}

	runs.forget()

	_, err = install.Install(windowsOptions(home, other, source, "", runs))
	if err != nil {
		t.Fatalf("установка со сменой настроек не прошла: %v", err)
	}

	if !runs.saw("schtasks /Create") {
		t.Fatalf("задача не переписана под другие настройки: %v", runs.calls)
	}
}

// TestWindowsNewBinaryRestartsTask: новый бинарник подхватывается только перезапуском задачи, и
// перезапуск ждёт, пока уйдёт процесс прежней сборки.
func TestWindowsNewBinaryRestartsTask(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	err = os.WriteFile(source, []byte("#!/bin/sh\necho новая сборка\n"), 0o755)
	if err != nil {
		t.Fatalf("новая сборка не записана: %v", err)
	}

	runs.forget()

	_, err = install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("повторная установка не прошла: %v", err)
	}

	if !runs.saw("schtasks /End") || !runs.saw("schtasks /Run") {
		t.Fatalf("служба не перезапущена под новый бинарник: %v", runs.calls)
	}

	if !runs.saw("tasklist") {
		t.Fatalf("перезапуск не дождался ухода процесса прежней сборки: %v", runs.calls)
	}
}

// TestWindowsUninstallRemovesTask: снятие убирает задачу и бинарник, а состояние и настройки
// оставляет: это данные владельца, а не следы установки.
func TestWindowsUninstallRemovesTask(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	paths, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	_, err = install.Uninstall(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("снятие не прошло: %v", err)
	}

	if !runs.saw("schtasks /Delete") {
		t.Fatalf("задача не снята: %v", runs.calls)
	}

	_, err = os.Stat(paths.Binary)
	if !os.IsNotExist(err) {
		t.Fatalf("бинарник остался: %v", err)
	}

	_, err = os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("снятие унесло настройки владельца: %v", err)
	}

	ready, _, err := install.Installed(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("проверка установки отказала: %v", err)
	}

	if ready {
		t.Fatal("снятая служба числится установленной")
	}
}

// TestWindowsUninstallWithoutTask: задачи не было — снятие подтверждает шаг, а не отказывает.
func TestWindowsUninstallWithoutTask(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Uninstall(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("снятие без задачи отказало: %v", err)
	}

	if runs.saw("schtasks /Delete") {
		t.Fatalf("снятие удаляло несуществующую задачу: %v", runs.calls)
	}
}

// TestWindowsBranchServiceHasOwnTask: у службы ветки своя задача и свой бинарник — правку проверяют
// рядом с рабочей службой, а не на живом канале владельца.
func TestWindowsBranchServiceHasOwnTask(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	working, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка рабочей службы не прошла: %v", err)
	}

	branch, err := install.Install(windowsOptions(home, cfgPath, source, "06-sluzhba", runs))
	if err != nil {
		t.Fatalf("установка службы ветки не прошла: %v", err)
	}

	if branch.Unit != "telegram-bridge-06-sluzhba" || branch.Unit == working.Unit {
		t.Fatalf("задача службы ветки не своя: %q против %q", branch.Unit, working.Unit)
	}

	if branch.Binary == working.Binary {
		t.Fatalf("бинарник службы ветки встал поверх рабочего: %q", branch.Binary)
	}
}

// TestWindowsArgumentsQuotePath: путь с пробелом в аргументах задачи идёт в кавычках, иначе служба
// получила бы два аргумента вместо пути.
func TestWindowsArgumentsQuotePath(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)

	spaced := filepath.Join(home, "мои настройки", "config.yaml")

	err := os.MkdirAll(filepath.Dir(spaced), 0o700)
	if err != nil {
		t.Fatalf("каталог настроек не создан: %v", err)
	}

	err = os.WriteFile(spaced, read(t, cfgPath), 0o600)
	if err != nil {
		t.Fatalf("настройки с пробелом в пути не записаны: %v", err)
	}

	runs := newScheduler()

	_, err = install.Install(windowsOptions(home, spaced, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	if !strings.Contains(taskText(t, runs.created), `serve --service --config "`+spaced+`"`) {
		t.Fatalf("путь с пробелом не взят в кавычки:\n%s", taskText(t, runs.created))
	}
}

// TestWindowsOneByteTaskXMLKeepsTask: запись задачи пришла однобайтной кодировкой консоли, а не
// UTF-16, — отпечаток всё равно находится, и живая служба не переписывается.
func TestWindowsOneByteTaskXMLKeepsTask(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	runs.forget()
	runs.singleByte = true

	_, err = install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("повторная установка не прошла: %v", err)
	}

	if runs.saw("schtasks /Create") {
		t.Fatalf("задача переписана из-за кодировки вывода: %v", runs.calls)
	}
}

// TestWindowsUnknownTaskXMLRefuses: вывод запроса задачи не XML в известной кодировке — отказ с
// причиной, а не «запись разошлась» и не перепись живого канала.
func TestWindowsUnknownTaskXMLRefuses(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	runs.forget()
	runs.garbage = true

	_, err = install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err == nil {
		t.Fatal("установка прошла, не прочитав запись задачи")
	}

	if !strings.Contains(err.Error(), "не как XML") {
		t.Fatalf("отказ не назвал кодировку вывода: %v", err)
	}

	if runs.saw("schtasks /Create") || runs.saw("schtasks /Run") {
		t.Fatalf("задача переписана при непрочитанной записи: %v", runs.calls)
	}
}

// TestWindowsHiddenTaskIsInstalled: задача не видна в общем списке — наличие всё равно отвечено
// вопросом по имени, и повторная установка не переписывает её.
func TestWindowsHiddenTaskIsInstalled(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	runs.forget()
	runs.hiddenFromList = true

	ready, _, err := install.Installed(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("наличие задачи не спрошено: %v", err)
	}

	if !ready {
		t.Fatal("задача, которой нет в списке, сочтена снятой")
	}

	_, err = install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("повторная установка не прошла: %v", err)
	}

	if runs.saw("schtasks /Create") {
		t.Fatalf("задача переписана из-за того, что её нет в списке: %v", runs.calls)
	}
}

// TestWindowsInstalledTellsQueryFailure: отказ списка задач — это «спросить не удалось», а не
// «задачи нет».
func TestWindowsInstalledTellsQueryFailure(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()
	runs.listFails = true

	ready, _, err := install.Installed(windowsOptions(home, cfgPath, source, "", runs))
	if err == nil {
		t.Fatalf("отказ списка задач принят за ответ: %v, готово %v", err, ready)
	}

	if !strings.Contains(err.Error(), "не спрошена ни по имени") {
		t.Fatalf("отказ не назвал, что задачи не спрошены: %v", err)
	}
}

// TestWindowsUninstallTellsQueryFailure: задача не спрошена — снятие отказывает с причиной. Тихое
// «автозапуска не было» оставило бы задачу, которая поднимет службу уже без бинарника.
func TestWindowsUninstallTellsQueryFailure(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()
	runs.listFails = true

	_, err := install.Uninstall(windowsOptions(home, cfgPath, source, "", runs))
	if err == nil {
		t.Fatal("снятие прошло, не спросив задачу")
	}

	if runs.saw("schtasks /Delete") {
		t.Fatalf("снятие удаляло задачу по непрочитанному ответу: %v", runs.calls)
	}
}

// TestWindowsRestartWaitsForFreshBuild: перезапуск подтверждается самой службой — её старт не
// раньше нынешнего бинарника. Первый ответ может быть прежним: установка спрашивает ещё раз.
//
// Оба сравниваемых времени задаёт стенд, а не часы прогона. Время нового бинарника ставится в миг
// запуска задачи: бинарник к нему уже на месте, а подтверждение перезапуска его время ещё не
// спросило. Время старта служба отвечает от того же мига. Возьми стенд время у часов — исход
// зависел бы от того, легли ли запись прежнего бинарника и запись нового в одну секунду.
func TestWindowsRestartWaitsForFreshBuild(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	err = os.WriteFile(source, []byte("#!/bin/sh\necho новая сборка\n"), 0o755)
	if err != nil {
		t.Fatalf("новая сборка не записана: %v", err)
	}

	// Доля секунды у времени бинарника не нулевая: сравнение, которое забыло привести время файла
	// к точности протокола, на нём краснеет.
	built := time.Date(2026, 10, 27, 12, 0, 0, 300_000_000, time.UTC)
	binary := filepath.Join(home, ".local", "bin", "telegram-bridge.exe")
	asked := 0

	again := windowsOptions(home, cfgPath, source, "", runs)
	again.RestartWait = time.Second
	again.Runner = func(name string, args ...string) (string, error) {
		if name == schtasksCommand && hasArg(args, "/Run") {
			err := os.Chtimes(binary, built, built)
			if err != nil {
				return "", fmt.Errorf("время бинарника не поставлено: %w", err)
			}
		}

		return runs.run(name, args...)
	}
	// Служба отвечает временем старта с точностью до секунды — как `serviceStarted` после разбора
	// `started_at`. Служба, поднятая в ту же секунду, что записан бинарник, — свежая.
	again.Started = func() (time.Time, bool) {
		asked++

		if asked == 1 {
			return built.Add(-time.Minute).Truncate(time.Second), true
		}

		return built.Truncate(time.Second), true
	}

	_, err = install.Install(again)
	if err != nil {
		t.Fatalf("установка после перезапуска отказала: %v", err)
	}

	if asked < 2 {
		t.Fatalf("установка поверила прежнему времени старта с первого ответа: вопросов %d", asked)
	}
}

// TestWindowsRestartTellsOldBuildApart: перезапуск не подхватил новую сборку — установка отказывает,
// а не печатает «служба идёт».
func TestWindowsRestartTellsOldBuildApart(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	err = os.WriteFile(source, []byte("#!/bin/sh\necho новая сборка\n"), 0o755)
	if err != nil {
		t.Fatalf("новая сборка не записана: %v", err)
	}

	old := time.Now().Add(-time.Hour)

	again := windowsOptions(home, cfgPath, source, "", runs)
	again.RestartWait = 50 * time.Millisecond
	again.Started = func() (time.Time, bool) { return old, true }

	_, err = install.Install(again)
	if err == nil {
		t.Fatal("установка прошла со службой прежней сборки")
	}

	if !strings.Contains(err.Error(), "не подхватил новую сборку") {
		t.Fatalf("отказ не назвал прежнюю сборку: %v", err)
	}
}

// TestWindowsRestartTellsSilence: служба не ответила после перезапуска — это отказ с причиной, а не
// подтверждение.
func TestWindowsRestartTellsSilence(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	err = os.WriteFile(source, []byte("#!/bin/sh\necho новая сборка\n"), 0o755)
	if err != nil {
		t.Fatalf("новая сборка не записана: %v", err)
	}

	again := windowsOptions(home, cfgPath, source, "", runs)
	again.RestartWait = 50 * time.Millisecond
	again.Started = func() (time.Time, bool) { return time.Time{}, false }

	_, err = install.Install(again)
	if err == nil {
		t.Fatal("установка прошла без ответа службы")
	}

	if !strings.Contains(err.Error(), "не ответила") {
		t.Fatalf("отказ не назвал молчание службы: %v", err)
	}
}

// TestWindowsUninstallWaitsForProcess: остановленная задача отпускает бинарник не сразу, и снятие
// ждёт, пока её процесс уйдёт.
func TestWindowsUninstallWaitsForProcess(t *testing.T) {
	t.Parallel()

	home, cfgPath, source := stand(t)
	runs := newScheduler()

	_, err := install.Install(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("установка не прошла: %v", err)
	}

	runs.forget()
	runs.lingering = 1

	_, err = install.Uninstall(windowsOptions(home, cfgPath, source, "", runs))
	if err != nil {
		t.Fatalf("снятие не прошло: %v", err)
	}

	if !runs.saw("schtasks /Delete") {
		t.Fatalf("задача не снята: %v", runs.calls)
	}
}

// newScheduler заводит пустой стенд планировщика: задачи нет, служба не идёт, отказов нет.
func newScheduler() *scheduler {
	return &scheduler{
		mu:             sync.Mutex{},
		calls:          nil,
		task:           "",
		created:        nil,
		running:        false,
		listFails:      false,
		xmlFails:       false,
		singleByte:     false,
		garbage:        false,
		hiddenFromList: false,
		lingering:      0,
	}
}

// windowsOptions собирает описание установки Windows на путях стенда.
func windowsOptions(home, cfgPath, source, instance string, runs *scheduler) install.Options {
	return install.Options{
		GOOS:        install.GOOSWindows,
		Home:        home,
		Instance:    instance,
		Config:      cfgPath,
		StateRoot:   "",
		Source:      source,
		Account:     ownerSID,
		BinDir:      "",
		AgentsDir:   "",
		Runner:      runs.run,
		Started:     nil,
		RestartWait: 0,
		Out:         nil,
	}
}

// taskText читает XML задачи, как его прочитает планировщик: UTF-16 с BOM. Кавычки в аргументах
// записаны XML-множеством `&#34;` — после разбора это те же кавычки.
func taskText(t *testing.T, data []byte) string {
	t.Helper()

	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xFE {
		t.Fatalf("задача записана не в UTF-16 с BOM: % X", data[:min(4, len(data))])
	}

	return strings.ReplaceAll(decodeTask(data), "&#34;", `"`)
}

// decodeTask читает XML задачи из UTF-16 с BOM — так его прочитает и планировщик.
func decodeTask(data []byte) string {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xFE {
		return string(data)
	}

	units := make([]uint16, 0, len(data)/2)

	for index := 2; index+1 < len(data); index += 2 {
		units = append(units, uint16(data[index])|uint16(data[index+1])<<8)
	}

	return string(utf16.Decode(units))
}

// hasArg отвечает, есть ли ключ среди аргументов вызова.
func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}

	return false
}

// argAfter отдаёт значение ключа: планировщику XML задачи передаётся путём к файлу.
func argAfter(args []string, key string) string {
	for index, arg := range args {
		if arg == key && index+1 < len(args) {
			return args[index+1]
		}
	}

	return ""
}
