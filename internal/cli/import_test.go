package cli_test

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/cli"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/legacy"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// skipReply — пропущенная тема в машинном отчёте переноса.
type skipReply struct {
	Name   string `json:"name"`
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// importReply — машинный отчёт переноса реестра тем.
type importReply struct {
	From    string      `json:"from"`
	Check   bool        `json:"check"`
	Read    int         `json:"read"`
	Placed  int         `json:"placed"`
	Skipped []skipReply `json:"skipped"`
	Offset  offsetReply `json:"offset"`
}

// offsetReply — перенос смещения опроса в машинном отчёте.
type offsetReply struct {
	From    string `json:"from"`
	Old     int64  `json:"old"`
	Service int64  `json:"service"`
	Outcome string `json:"outcome"`
}

// oldOffsetValue — смещение опроса старого моста на стенде: так его пишет он сам, голым числом.
const oldOffsetValue = 709112834

// TestImportTopicsPortsOffset: вместе с реестром переносится смещение опроса старого моста, и
// служба начнёт с него первый опрос — обновление, разобранное старым мостом, но не подтверждённое,
// второй раз не придёт. Второй запуск смещения не трогает: оно уже то же, а файл не пересоздаётся.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestImportTopicsPortsOffset(t *testing.T) {
	root := t.TempDir()
	_, cfgPath := settings(t, root)

	old := oldState(t, `{"мост": 101}`)
	oldOffset(t, old, strconv.Itoa(oldOffsetValue))

	var answer importReply

	decode(t, run(t, 0, "import-topics", "--config", cfgPath, "--from", old, "--json"), &answer)

	if answer.Offset.Outcome != "placed" || answer.Offset.Old != oldOffsetValue || answer.Offset.Service != 0 {
		t.Fatalf("перенос не назвал смещения: %+v", answer.Offset)
	}

	if got := serviceOffset(t, cfgPath); got != oldOffsetValue {
		t.Fatalf("смещение службы %d, а старый мост оставил %d", got, oldOffsetValue)
	}

	laid := statFile(t, updatesFile(t, cfgPath))

	decode(t, run(t, 0, "import-topics", "--config", cfgPath, "--from", old, "--json"), &answer)

	if answer.Offset.Outcome != "kept" {
		t.Fatalf("второй запуск снова переносил смещение: %+v", answer.Offset)
	}

	assertKeptFile(t, updatesFile(t, cfgPath), laid)
}

// TestImportTopicsRefusesBrokenOffset: битое смещение старого моста — отказ с причиной до записи:
// ни реестра, ни смещения у службы не появляется, хотя реестр старого моста целый.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestImportTopicsRefusesBrokenOffset(t *testing.T) {
	root := t.TempDir()
	_, cfgPath := settings(t, root)

	old := oldState(t, `{"мост": 101}`)
	oldOffset(t, old, "сто")

	_ = run(t, 1, "import-topics", "--config", cfgPath, "--from", old, "--json")

	for _, path := range []string{registryFile(t, cfgPath), updatesFile(t, cfgPath)} {
		_, err := os.Stat(path)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("битое смещение перенесено наполовину: %s, %v", path, err)
		}
	}
}

// TestImportTopicsRequiresFrom: каталог старого моста называет только `--from` — умолчания у
// команды нет, и вызов без флага кончается негодным вызовом, называя флаг.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestImportTopicsRequiresFrom(t *testing.T) {
	root := t.TempDir()
	_, cfgPath := settings(t, root)

	_, failures := runIO(t, cli.ExitUsage, "", "import-topics", "--config", cfgPath, "--json")

	if !strings.Contains(failures, "--from") {
		t.Fatalf("отказ не назвал флаг --from: %q", failures)
	}
}

// TestImportTopicsPortsOldRegistry: перенос на поддельном состоянии старого моста кладёт темы в
// состояние службы, а второй запуск ничего не портит и не плодит. Старое состояние при этом
// остаётся байт в байт: перенос только читает его.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestImportTopicsPortsOldRegistry(t *testing.T) {
	root := t.TempDir()
	_, cfgPath := settings(t, root)

	old := oldState(t, `{"мост": 101, "шестой-этап": 202}`)
	kept := readFile(t, filepath.Join(old, legacy.TopicsFile))

	var answer importReply

	out := run(t, 0, "import-topics", "--config", cfgPath, "--from", old, "--json")
	decode(t, out, &answer)

	if answer.Read != 2 || answer.Placed != 2 || len(answer.Skipped) != 0 {
		t.Fatalf("перенос не назвал своего исхода: %+v", answer)
	}

	assertRegistry(t, cfgPath, map[string]int64{"мост": 101, "шестой-этап": 202})

	laid := statFile(t, registryFile(t, cfgPath))

	out = run(t, 0, "import-topics", "--config", cfgPath, "--from", old, "--json")
	decode(t, out, &answer)

	assertRepeated(t, answer)
	assertRegistry(t, cfgPath, map[string]int64{"мост": 101, "шестой-этап": 202})
	assertKeptFile(t, registryFile(t, cfgPath), laid)

	if string(kept) != string(readFile(t, filepath.Join(old, legacy.TopicsFile))) {
		t.Fatal("перенос переписал состояние старого моста")
	}
}

// TestImportTopicsCheckWritesNothing: ключ `--check` называет, чего в реестре службы не хватает, и
// не пишет ничего: им владелец проверяет уже перенесённое.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestImportTopicsCheckWritesNothing(t *testing.T) {
	root := t.TempDir()
	_, cfgPath := settings(t, root)

	old := oldState(t, `{"мост": 101}`)
	oldOffset(t, old, strconv.Itoa(oldOffsetValue))

	var answer importReply

	out := run(t, 0, "import-topics", "--config", cfgPath, "--from", old, "--check", "--json")
	decode(t, out, &answer)

	if !answer.Check || answer.Read != 1 || answer.Placed != 1 || answer.Offset.Outcome != "placed" {
		t.Fatalf("сверка не назвала недостающего: %+v", answer)
	}

	topics := serviceTopics(t, cfgPath)

	if len(topics.ByName) != 0 {
		t.Fatalf("сверка записала реестр: %+v", topics)
	}

	_, err := os.Stat(updatesFile(t, cfgPath))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("сверка записала смещение опроса: %v", err)
	}
}

// TestImportTopicsRefusesLiveService: при идущей службе перенос отказывается. Реестр тем служба
// держит в памяти и переписывает файл при первом изменении, поэтому положенное поверх неё пропало
// бы — тихо и вместе с последним перенесённым.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestImportTopicsRefusesLiveService(t *testing.T) {
	cfgPath, stop := service(t)
	defer stop()

	old := oldState(t, `{"мост": 101}`)

	_ = run(t, 1, "import-topics", "--config", cfgPath, "--from", old, "--json")

	_, err := os.Stat(registryFile(t, cfgPath))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("перенос при живой службе всё же записал реестр: %v", err)
	}
}

// TestImportTopicsRefusesBrokenState: битое состояние — внятный отказ, и ни половины перенесённого:
// реестр службы не заводится вовсе, хотя одна тема в старом состоянии целая.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestImportTopicsRefusesBrokenState(t *testing.T) {
	root := t.TempDir()
	_, cfgPath := settings(t, root)

	old := oldState(t, `{"мост": 101, "битая": "не id"}`)

	_ = run(t, 1, "import-topics", "--config", cfgPath, "--from", old, "--json")

	_, err := os.Stat(registryFile(t, cfgPath))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("битое состояние перенесено наполовину: %v", err)
	}
}

// TestImportTopicsRefusesSilentService: служба есть, но за срок не ответила — это отказ, а не
// «службы нет». Молчащая живая служба держит реестр тем в памяти так же, как говорящая: она может
// быть занята записью состояния на нагруженном диске, и запись поверх неё пропала бы при первом её
// сохранении.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestImportTopicsRefusesSilentService(t *testing.T) {
	root := t.TempDir()
	socket := silentService(t)
	cfgPath := settingsFor(t, root, root, socket)

	old := oldState(t, `{"мост": 101}`)

	_ = run(t, 1, "import-topics", "--config", cfgPath, "--from", old, "--json")

	_, err := os.Stat(registryFile(t, cfgPath))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("молчащая служба принята за снятую: перенос записал реестр, %v", err)
	}
}

// TestImportTopicsRefusesOwnState: источник и цель переноса — один файл, когда корень состояния
// службы указывает на каталог старого моста. Тогда запись нового формата легла бы поверх реестра
// старого моста, и откатываться было бы некуда.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestImportTopicsRefusesOwnState(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "tools", "telegram", "state")
	cfgPath := settingsFor(t, root, stateRoot, sockPath(t))

	err := os.MkdirAll(stateRoot, store.DirPerm)
	if err != nil {
		t.Fatalf("каталог старого моста не заведён: %v", err)
	}

	body := `{"мост": 101}`
	registry := filepath.Join(stateRoot, legacy.TopicsFile)

	err = os.WriteFile(registry, []byte(body), 0o600)
	if err != nil {
		t.Fatalf("рейстр старого моста не записан: %v", err)
	}

	// --from называет тот же каталог, куда смотрит корень состояния службы.
	_ = run(t, 1, "import-topics", "--config", cfgPath, "--from", stateRoot, "--json")

	if string(readFile(t, registry)) != body {
		t.Fatal("перенос записал поверх реестра старого моста")
	}

	_, err = legacy.ReadTopics(registry)
	if err != nil {
		t.Fatalf("рейстр старого моста больше не разбирается: %v", err)
	}
}

// TestImportTopicsWritesNothingWithoutTopics: переносить нечего — файл реестра службы не заводится
// вовсе. Пустой реестр на диске значил бы «темы были и кончились», а их не было.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestImportTopicsWritesNothingWithoutTopics(t *testing.T) {
	root := t.TempDir()
	_, cfgPath := settings(t, root)

	old := oldState(t, `{}`)

	var answer importReply

	out := run(t, 0, "import-topics", "--config", cfgPath, "--from", old, "--json")
	decode(t, out, &answer)

	if answer.Read != 0 || answer.Placed != 0 || len(answer.Skipped) != 0 {
		t.Fatalf("пустой старый реестр назван не пустым: %+v", answer)
	}

	_, err := os.Stat(registryFile(t, cfgPath))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("перенос завёл файл реестра на пустом старом реестре: %v", err)
	}

	// Смещения в старом состоянии нет — переносить нечего, и это не отказ.
	if answer.Offset.Outcome != "none" {
		t.Fatalf("смещение без файла названо не отсутствующим: %+v", answer.Offset)
	}

	_, err = os.Stat(updatesFile(t, cfgPath))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("перенос завёл смещение опроса без смещения в старом состоянии: %v", err)
	}
}

// silentService поднимает сокет, который принимает соединения и не отвечает ни одним кадром: так
// выглядит живая служба, занятая записью состояния на нагруженном диске.
func silentService(t *testing.T) string {
	t.Helper()

	path := sockPath(t)

	listener, err := listen(t, path)
	if err != nil {
		t.Fatalf("молчащий сокет не поднят: %v", err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			// Кадры читаются и выбрасываются: соединение принимается, ответа нет ни одного.
			go func() {
				defer func() { _ = conn.Close() }()

				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()

	return path
}

// listen поднимает сокет стенда: адрес у него свой, а отмена — конца теста.
func listen(t *testing.T, path string) (net.Listener, error) {
	t.Helper()

	cfg := net.ListenConfig{} //nolint:exhaustruct // значений по умолчанию хватает

	listener, err := cfg.Listen(t.Context(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("сокет %s не поднят: %w", path, err)
	}

	return listener, nil
}

// statFile отдаёт сведения о файле — по ним видно, пересоздан он или остался прежним.
func statFile(t *testing.T, path string) os.FileInfo {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("файл не осмотрен: %v", err)
	}

	return info
}

// assertKeptFile сверяет, что файл тот же самый: запись реестра идёт временным файлом с
// переименованием, поэтому переписанный файл — всегда другой.
func assertKeptFile(t *testing.T, path string, kept os.FileInfo) {
	t.Helper()

	if !os.SameFile(kept, statFile(t, path)) {
		t.Fatal("перенос переписал файл реестра, хотя менять было нечего")
	}
}

// assertRegistry сверяет реестр тем службы: каждая перенесённая тема на месте, и тем ровно
// столько, сколько было в старом реестре.
func assertRegistry(t *testing.T, cfgPath string, want map[string]int64) {
	t.Helper()

	topics := serviceTopics(t, cfgPath)

	for name, id := range want {
		topic, ok := topics.ByNameLookup(name)
		if !ok || topic.ID != id {
			t.Fatalf("тема «%s» не легла в реестр службы: %+v", name, topic)
		}
	}

	if len(topics.ByName) != len(want) || len(topics.ByID) != len(want) {
		t.Fatalf("в реестре службы не те темы: %+v", topics)
	}
}

// assertRepeated сверяет отчёт повторного переноса: положено ноль, и каждый пропуск объяснён.
func assertRepeated(t *testing.T, answer importReply) {
	t.Helper()

	if answer.Read != 2 || answer.Placed != 0 || len(answer.Skipped) != 2 {
		t.Fatalf("второй перенос не назвал себя повторным: %+v", answer)
	}

	for _, skip := range answer.Skipped {
		if skip.Reason != string(legacy.ReasonInPlace) || skip.Detail == "" {
			t.Fatalf("повторный перенос не объяснил пропуск: %+v", skip)
		}
	}
}

// oldState кладёт поддельное состояние старого моста и отдаёт его каталог.
func oldState(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, legacy.TopicsFile), []byte(body), 0o600)
	if err != nil {
		t.Fatalf("состояние старого моста не записано: %v", err)
	}

	return dir
}

// oldOffset кладёт смещение опроса в состояние старого моста.
func oldOffset(t *testing.T, dir, body string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, legacy.OffsetFile), []byte(body), 0o600)
	if err != nil {
		t.Fatalf("смещение старого моста не записано: %v", err)
	}
}

// serviceOffset читает смещение опроса службы по её настройкам.
func serviceOffset(t *testing.T, cfgPath string) int64 {
	t.Helper()

	updates, err := store.LoadUpdates(openState(t, cfgPath))
	if err != nil {
		t.Fatalf("смещение опроса службы не прочитано: %v", err)
	}

	return updates.Offset
}

// updatesFile отдаёт путь файла смещения опроса службы.
func updatesFile(t *testing.T, cfgPath string) string {
	t.Helper()

	return openState(t, cfgPath).Path("updates.json")
}

// serviceTopics читает реестр тем службы по её настройкам.
func serviceTopics(t *testing.T, cfgPath string) *store.Topics {
	t.Helper()

	topics, err := store.LoadTopics(openState(t, cfgPath))
	if err != nil {
		t.Fatalf("реестр тем службы не прочитан: %v", err)
	}

	return topics
}

// registryFile отдаёт путь файла реестра тем службы.
func registryFile(t *testing.T, cfgPath string) string {
	t.Helper()

	return openState(t, cfgPath).Path(legacy.TopicsFile)
}

// openState открывает каталог состояния службы по её настройкам.
func openState(t *testing.T, cfgPath string) *store.Store {
	t.Helper()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	st, err := store.Open(cfg.StateRoot)
	if err != nil {
		t.Fatalf("состояние службы не открыто: %v", err)
	}

	return st
}

// readFile читает файл целиком.
func readFile(t *testing.T, path string) []byte {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("файл не прочитан: %v", err)
	}

	return body
}
