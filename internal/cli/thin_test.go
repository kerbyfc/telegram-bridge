package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/cli"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/core"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/server"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// session — id сессии в стенде: тот же, который клиент кладёт в свой вход.
const session = "ses-thin"

// awayReply — машинный ответ подкоманд режима, как его читает команда режима.
//
//nolint:tagliatelle // имена полей заданы контрактом моста и пишутся как в нём
type awayReply struct {
	Mode      string `json:"mode"`
	TopicID   int64  `json:"topic_id"`
	TopicName string `json:"topic_name"`
	Created   bool   `json:"created"`
}

// askReply — машинный ответ вопроса с вариантами.
//
//nolint:tagliatelle // имена полей заданы контрактом моста и пишутся как в нём
type askReply struct {
	MessageID  int   `json:"message_id"`
	MessageIDs []int `json:"message_ids"`
}

// checkReply — одна проверка самопроверки. Исходов три: подтверждено, не подтверждено, отказ.
type checkReply struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// checkFailed — исход проверки «отказ», как его пишет машинный ответ.
const checkFailed = "failed"

// doctorReply — машинный ответ самопроверки.
type doctorReply struct {
	OK     bool         `json:"ok"`
	Checks []checkReply `json:"checks"`
}

// stubAPI — поддельный Bot API: вызовы платформы в тестах не делаются вовсе, а долгий опрос
// просто ждёт конца стенда.
type stubAPI struct{}

func (stubAPI) SendMessage(_ context.Context, _ int64, _ string) (int, error) { return 7, nil }
func (stubAPI) EditMessageText(_ context.Context, _ int, _ string) error      { return nil }
func (stubAPI) CreateTopic(_ context.Context, _ string) (int64, error)        { return 42, nil }
func (stubAPI) RenameTopic(_ context.Context, _ int64, _ string) error        { return nil }
func (stubAPI) CloseTopic(_ context.Context, _ int64) error                   { return nil }
func (stubAPI) ReopenTopic(_ context.Context, _ int64) error                  { return nil }
func (stubAPI) SetTopicInMode(_ context.Context, _ int64, _ bool) error       { return nil }
func (stubAPI) SetReaction(_ context.Context, _ int, _ botapi.Mark) error     { return nil }
func (stubAPI) PinMessage(_ context.Context, _ int64, _ int) error            { return nil }
func (stubAPI) SendChatAction(_ context.Context, _ int64, _ string) error     { return nil }

// GetUpdates ждёт конца стенда: обновлений в тесте тонкого клиента не бывает.
func (stubAPI) GetUpdates(ctx context.Context, _ int64, timeout time.Duration) ([]botapi.Update, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil, context.Canceled
	case <-timer.C:
		return nil, nil
	}
}

// TestThinClientDrivesMode: короткий вызов включает режим, называет его состояние и снимает —
// вместо запуска моста на каждый чих. Процесс живёт один вызов и ход сессии не держит.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestThinClientDrivesMode(t *testing.T) {
	cfgPath, stop := service(t)
	defer stop()

	var answer awayReply

	out := run(t, 0, "away", "on", "шестой-этап", "--session", session, "--config", cfgPath, "--json")
	decode(t, out, &answer)

	if answer.Mode != string(protocol.ModeOn) || answer.TopicName != "шестой-этап" {
		t.Fatalf("режим не включён: %+v", answer)
	}

	if !answer.Created || answer.TopicID != 42 {
		t.Fatalf("тема не заведена: %+v", answer)
	}

	out = run(t, 0, "away", "status", "--session", session, "--config", cfgPath, "--json")
	decode(t, out, &answer)

	if answer.Mode != string(protocol.ModeOn) || answer.TopicName != "шестой-этап" {
		t.Fatalf("состояние режима не то: %+v", answer)
	}

	out = run(t, 0, "away", "off", "--session", session, "--config", cfgPath, "--json")
	decode(t, out, &answer)

	if answer.Mode != string(protocol.ModeOff) {
		t.Fatalf("режим не снят: %+v", answer)
	}
}

// TestThinClientRefusesWithoutSession: id сессии называет клиент, из окружения он не выводится —
// имя файла стенограммы и переменная окружения совпадают с ним не всегда. Переменные сессии обоих
// клиентов в окружении стоят, и вызов без ключа всё равно отказывает.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой, и переменные сессии тест ставит на весь процесс.
func TestThinClientRefusesWithoutSession(t *testing.T) {
	t.Setenv("OPENCODE_SESSION_ID", "oc-session")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "cc-session")

	cfgPath, stop := service(t)
	defer stop()

	_ = run(t, 2, "away", "on", "--config", cfgPath, "--json")
}

// TestAskHandlesRefusalAndNumbersOptions: второй вход сессии — вопрос с вариантами. Нумерацию
// делает служба, а отказ обработан: он уезжает ненулевым кодом возврата, а не теряется.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestAskHandlesRefusalAndNumbersOptions(t *testing.T) {
	cfgPath, stop := service(t)
	defer stop()

	// Режима у сессии нет — служба отказывает, и подкоманда называет это кодом возврата.
	_ = run(t, 1, "ask", "как быть?", "--option", "а=доделать", "--session", session,
		"--config", cfgPath, "--json")

	_ = run(t, 0, "away", "on", "вопросы", "--session", session, "--config", cfgPath, "--json")

	var answer askReply

	out := run(t, 0, "ask", "как быть?", "--option", "а=доделать", "--option", "б=отложить",
		"--session", session, "--config", cfgPath, "--json")
	decode(t, out, &answer)

	if answer.MessageID == 0 || len(answer.MessageIDs) == 0 {
		t.Fatalf("вопрос не отправлен: %+v", answer)
	}

	// Негодный вариант до службы не доходит вовсе: это негодный вызов.
	_ = run(t, 2, "ask", "как быть?", "--option", "безключа", "--session", session,
		"--config", cfgPath, "--json")
}

// TestDoctorNamesDeadService: падение службы видно самопроверкой, а не молчанием темы: сокета нет
// — проверка службы провалена, и код возврата это называет.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestDoctorNamesDeadService(t *testing.T) {
	cfgPath, stop := service(t)
	stop()

	// Дом назван нарочно: без него самопроверка смотрела бы запись автозапуска в настоящем
	// каталоге агентов владельца, а тест туда не ходит ни на чтение.
	out := run(t, 1, "doctor", "--config", cfgPath, "--home", filepath.Dir(cfgPath), "--json")

	var answer doctorReply

	decode(t, out, &answer)

	if answer.OK {
		t.Fatalf("самопроверка без службы ответила «всё хорошо»: %+v", answer)
	}

	for _, want := range []string{"настройки", "состояние", "журнал", "автозапуск", "служба"} {
		if named(answer, want).Detail == "проверки нет" {
			t.Fatalf("самопроверка не назвала %q: %+v", want, answer.Checks)
		}
	}

	if named(answer, "настройки").State != "ok" {
		t.Fatalf("годные настройки названы негодными: %+v", answer.Checks)
	}

	// Telegram со стенда недоступен: значки не подтверждены, но и не негодны.
	if icons := named(answer, "значки тем"); icons.State != "unknown" {
		t.Fatalf("недоступность Telegram названа иначе: %+v", icons)
	}

	dead := named(answer, "служба")
	if dead.State != checkFailed {
		t.Fatalf("мёртвая служба названа живой: %+v", dead)
	}

	// Паника уезжает в вывод процесса, а не в журнал: самопроверка обязана назвать и его путь.
	if !strings.Contains(named(answer, "журнал").Detail, "вывод") {
		t.Fatalf("вывод процесса службы не назван: %q", named(answer, "журнал").Detail)
	}
}

// TestDoctorSeesLiveService: живая служба отвечает самопроверке сама — живость, исход опроса и
// соединения сессий.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestDoctorSeesLiveService(t *testing.T) {
	cfgPath, stop := service(t)
	defer stop()

	_ = run(t, 0, "away", "on", "самопроверка", "--session", session, "--config", cfgPath, "--json")

	// Дом назван нарочно: без него самопроверка смотрела бы запись автозапуска в настоящем
	// каталоге агентов владельца, а тест туда не ходит ни на чтение.
	out := run(t, 1, "doctor", "--config", cfgPath, "--home", filepath.Dir(cfgPath), "--json")

	var answer doctorReply

	decode(t, out, &answer)

	live := named(answer, "служба")
	if live.State != "ok" || !strings.Contains(live.Detail, "протокол v") {
		t.Fatalf("живая служба не названа: %+v", live)
	}

	seen := named(answer, "сессии")
	if seen.State != "ok" || !strings.Contains(seen.Detail, session) {
		t.Fatalf("соединение сессии не названо: %+v", seen)
	}

	// Первый долгий опрос в стенде не возвращается вовсе (поллер не поднят), и это «не
	// подтверждено», а не отказ: иначе владелец получал бы ненулевой код на здоровой службе
	// ровно тогда, когда отчёт установки советует позвать самопроверку.
	poll := named(answer, "бот и группа")
	if poll.State != "unknown" {
		t.Fatalf("неподтверждённый опрос назван отказом: %+v", poll)
	}

	// Единственный отказ здесь — отсутствие установки. Поставим запись автозапуска и бинарник, и код
	// возврата станет нулевым, хотя опрос так и не подтверждён.
	placeUnit(t, filepath.Dir(cfgPath))
	placeServiceBinary(t, filepath.Dir(cfgPath))

	out = run(t, 0, "doctor", "--config", cfgPath, "--home", filepath.Dir(cfgPath), "--json")
	decode(t, out, &answer)

	if !answer.OK {
		t.Fatalf("неподтверждённая проверка уронила вердикт: %+v", answer)
	}
}

// TestDoctorNamesServiceWithoutBinary: бинарник, снесённый руками мимо снятия, служба переживает,
// а звонящие его не находят. Самопроверка называет это прямо — «служба идёт, а бинарника нет» — и
// говорит, чем это чинится. Без службы та же запись без бинарника — другая беда: поднимать службу
// нечем.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestDoctorNamesServiceWithoutBinary(t *testing.T) {
	cfgPath, stop := service(t)
	home := filepath.Dir(cfgPath)

	placeUnit(t, home)

	var answer doctorReply

	decode(t, run(t, 1, "doctor", "--config", cfgPath, "--home", home, "--json"), &answer)

	gone := named(answer, "автозапуск")
	for _, want := range []string{"служба идёт, а её бинарника нет", "`install`", "`uninstall`"} {
		if gone.State != checkFailed || !strings.Contains(gone.Detail, want) {
			t.Fatalf("служба без бинарника не названа (%q): %+v", want, gone)
		}
	}

	stop()

	decode(t, run(t, 1, "doctor", "--config", cfgPath, "--home", home, "--json"), &answer)

	gone = named(answer, "автозапуск")
	if gone.State != checkFailed || !strings.Contains(gone.Detail, "запись автозапуска есть, а бинарника нет") {
		t.Fatalf("запись без бинарника и без службы не названа: %+v", gone)
	}
}

// placeUnit кладёт запись автозапуска туда, где её ищет самопроверка с этим домом.
func placeUnit(t *testing.T, home string) {
	t.Helper()

	placeFile(t, filepath.Join(home, "Library", "LaunchAgents", "com.kerbyfc.telegram-bridge.plist"),
		"<plist/>\n")
}

// placeServiceBinary кладёт бинарник службы туда, откуда его поднимает запись автозапуска.
func placeServiceBinary(t *testing.T, home string) {
	t.Helper()

	placeFile(t, filepath.Join(home, ".local", "bin", "telegram-bridge"), "#!/bin/sh\n")
}

// placeFile кладёт файл стенда вместе с каталогом.
func placeFile(t *testing.T, path, body string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		t.Fatalf("каталог %s не создан: %v", filepath.Dir(path), err)
	}

	err = os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatalf("файл %s не положен: %v", path, err)
	}
}

// TestDoctorReconcilesSessionID: сверка id сессии. `/clear` кончает разговор, но не процесс, и мод
// нарочно не говорит службе «прощай» — иначе канал умер бы раньше клиента, — поэтому служба может
// знать сессию под прежним id. Сверка называет это прямо, а какая из сессий «та же» — не
// угадывает: связать два id нечем.
//
// Параллельным этот тест быть не может: подкоманда печатает в os.Stdout, а стенд подменяет его
// своей трубой — на двух тестах сразу выдача ушла бы в чужую трубу.
//
//nolint:paralleltest // стенд подменяет os.Stdout: двум тестам сразу его не поделить
func TestDoctorReconcilesSessionID(t *testing.T) {
	cfgPath, stop := service(t)
	defer stop()

	_ = run(t, 0, "away", "on", "сверка", "--session", session, "--config", cfgPath, "--json")

	var answer doctorReply

	home := filepath.Dir(cfgPath)

	out := run(t, 1, "doctor", "--config", cfgPath, "--home", home, "--session", session, "--json")
	decode(t, out, &answer)

	same := named(answer, "id сессии")
	if same.State != "ok" || !strings.Contains(same.Detail, session) {
		t.Fatalf("сверка не признала свой же id: %+v", same)
	}

	if !strings.Contains(same.Detail, "вклад ") {
		t.Fatalf("сверка не назвала вклад в статусную строку: %q", same.Detail)
	}

	out = run(t, 1, "doctor", "--config", cfgPath, "--home", home, "--session", "после-clear", "--json")
	decode(t, out, &answer)

	other := named(answer, "id сессии")
	if other.State != checkFailed {
		t.Fatalf("сверка признала чужой id своим: %+v", other)
	}

	if !strings.Contains(other.Detail, session) || !strings.Contains(other.Detail, "clear") {
		t.Fatalf("сверка не назвала расхождение id: %q", other.Detail)
	}
}

// service поднимает службу стенда и отдаёт путь к её настройкам. Ни одного адреса системы
// владельца здесь нет: состояние, сокет и корень вклада в статусную строку — свои на тест.
func service(t *testing.T) (string, func()) {
	t.Helper()

	return serviceWith(t, stubAPI{})
}

// serviceWith поднимает службу стенда с названным Bot API: так тест считает вызовы платформы —
// сколько сообщений ушло в тему и что в них.
func serviceWith(t *testing.T, api botapi.API) (string, func()) {
	t.Helper()

	return serviceFor(t, api, session)
}

// serviceFor — та же служба стенда с модом сессии `sessionID`: снимок нагрузки хука называет свою
// сессию, и её мод обязан быть на связи, иначе `mode.on` отказывает.
func serviceFor(t *testing.T, api botapi.API, sessionID string) (string, func()) {
	t.Helper()

	root := t.TempDir()
	socket, cfgPath := settings(t, root)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	st, err := store.Open(cfg.StateRoot)
	if err != nil {
		t.Fatalf("состояние не открыто: %v", err)
	}

	jrnl, err := store.OpenJournal(cfg.Log(), cfg.LogMaxBytes, cfg.LogKeep)
	if err != nil {
		t.Fatalf("журнал не открыт: %v", err)
	}

	engine, err := core.New(cfg, api, st, jrnl)
	if err != nil {
		t.Fatalf("ядро не поднялось: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	srv, err := server.Listen(ctx, socket, engine)
	if err != nil {
		t.Fatalf("сокет не поднят: %v", err)
	}

	var serving sync.WaitGroup

	serving.Add(1)

	go func() {
		defer serving.Done()

		_ = srv.Serve(ctx)
	}()

	// Сессия приветствуется модом: без мода `mode.on` отказывает — это поломка установки, а не
	// рабочий случай.
	mod, err := greet(ctx, socket, sessionID)
	if err != nil {
		t.Fatalf("мод не подключился: %v", err)
	}

	once := sync.Once{}

	return cfgPath, func() {
		once.Do(func() {
			mod()
			cancel()
			srv.Close()
			serving.Wait()

			_ = jrnl.Close()
		})
	}
}

// settings пишет файл настроек стенда и отдаёт адрес сокета и путь настроек.
func settings(t *testing.T, root string) (string, string) {
	t.Helper()

	socket := sockPath(t)

	return socket, settingsFor(t, root, root, socket)
}

// settingsFor пишет файл настроек стенда с названными корнем состояния и адресом сокета. Переносу
// реестра нужны настройки, у которых корень состояния бывает и не тем, что каталог стенда.
func settingsFor(t *testing.T, root, stateRoot, socket string) string {
	t.Helper()

	cfgPath := filepath.Join(root, "config.yaml")
	body := strings.Join([]string{
		"bot_token: \"1234567:секрет\"",
		"group_id: -1001234567890",
		"owner_id: 11111111",
		"state_root: " + stateRoot,
		"default_topic_name: по-умолчанию",
		"statusline_root: " + filepath.Join(root, "statusline"),
		"socket_path: " + socket,
		"poll_timeout: 50ms",
		// Прокси в закрытый порт: сверка значков самопроверки в настоящий Telegram со стенда не ходит.
		"proxy: http://127.0.0.1:1",
		"retry_pauses: [\"1ms\"]",
		"",
	}, "\n")

	err := os.WriteFile(cfgPath, []byte(body), 0o600)
	if err != nil {
		t.Fatalf("настройки не записаны: %v", err)
	}

	return cfgPath
}

// sockPath отдаёт короткий адрес сокета стенда: предел длины адреса Unix-сокета меньше длины
// временного каталога теста на macOS.
func sockPath(t *testing.T) string {
	t.Helper()

	//nolint:usetesting // t.TempDir даёт путь длиннее предела адреса Unix-сокета на macOS
	sockDir, err := os.MkdirTemp("/tmp", "tgb")
	if err != nil {
		t.Fatalf("каталог сокета не создан: %v", err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	return filepath.Join(sockDir, "bridge.sock")
}

// greet открывает соединение сессии: роль `session` держит средство сессии, и без него служба
// сессии не знает. `sessionID` — та сессия, которую занимает мод: снимок нагрузки хука называет
// свою, и режим включается ей же.
func greet(ctx context.Context, socket, sessionID string) (func(), error) {
	conn, err := dial(ctx, socket)
	if err != nil {
		return nil, err
	}

	hello := protocol.Request{ //nolint:exhaustruct // приветствию хватает роли, вида, сессии и клиента
		V: protocol.Version, Op: protocol.OpHello, RID: "h-1",
		Role: protocol.RoleSession, Kind: string(protocol.KindMod),
		Session: sessionID, Client: string(protocol.ClientClaudeCode),
	}

	err = protocol.NewWriter(conn).WriteRequest(hello)
	if err != nil {
		return nil, fmt.Errorf("приветствие мода не отправлено: %w", err)
	}

	_, err = protocol.NewReader(conn).ReadResponse()
	if err != nil {
		return nil, fmt.Errorf("ответ на приветствие не прочитан: %w", err)
	}

	return func() { _ = conn.Close() }, nil
}

// named достаёт проверку самопроверки по имени; её нет — проваленная заглушка с этим именем.
func named(answer doctorReply, name string) checkReply {
	for _, one := range answer.Checks {
		if one.Name == name {
			return one
		}
	}

	return checkReply{Name: name, State: checkFailed, Detail: "проверки нет"}
}

// dial открывает соединение с сокетом службы.
func dial(ctx context.Context, socket string) (net.Conn, error) {
	dialer := net.Dialer{} //nolint:exhaustruct // соединению хватает значений по умолчанию

	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("сокет службы не отвечает: %w", err)
	}

	return conn, nil
}

// run исполняет подкоманду и сверяет код возврата, отдавая её выдачу.
func run(t *testing.T, want int, args ...string) string {
	t.Helper()

	out, _ := runIO(t, want, "", args...)

	return out
}

// runIO исполняет подкоманду с названным стандартным входом и отдаёт её выдачу и её ошибки: у
// подкоманд хуков вход и выход и есть протокол, и молчание на месте решения — тоже находка.
func runIO(t *testing.T, want int, stdin string, args ...string) (string, string) {
	t.Helper()

	keptIn, keptOut, keptErr := os.Stdin, os.Stdout, os.Stderr

	inReader, inWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("труба входа не поднята: %v", err)
	}

	_, err = inWriter.WriteString(stdin)
	if err != nil {
		t.Fatalf("нагрузка на вход не положена: %v", err)
	}

	_ = inWriter.Close()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("труба выдачи не поднята: %v", err)
	}

	errReader, errWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("труба ошибок не поднята: %v", err)
	}

	os.Stdin, os.Stdout, os.Stderr = inReader, writer, errWriter

	code := cli.Run(args)

	_ = writer.Close()
	_ = errWriter.Close()

	os.Stdin, os.Stdout, os.Stderr = keptIn, keptOut, keptErr

	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("выдача не прочитана: %v", err)
	}

	failures, err := io.ReadAll(errReader)
	if err != nil {
		t.Fatalf("ошибки не прочитаны: %v", err)
	}

	if code != want {
		t.Fatalf("код возврата %d, а ждали %d; выдача: %s; ошибки: %s", code, want, out, failures)
	}

	return string(out), string(failures)
}

// decode разбирает машинный ответ подкоманды.
func decode(t *testing.T, out string, into any) {
	t.Helper()

	err := json.Unmarshal([]byte(strings.TrimSpace(out)), into)
	if err != nil {
		t.Fatalf("машинный ответ не разобран (%q): %v", out, err)
	}
}
