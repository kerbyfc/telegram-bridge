package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// hookBlocked — код возврата хука, который блокирует вызов инструмента и отдаёт модели причину.
const hookBlocked = 2

// askFixture — путь к снимку вызова `AskUserQuestion` в наборе живых нагрузок. Вход хука в тестах
// берётся оттуда, а не литералом: переименованное поле клиента обязано покраснить прогон.
const askFixture = "../hook/testdata/PreToolUse.ask-user-question.json"

// countedAPI — поддельный Bot API стенда, который помнит отправленные тексты: по ним видно, сколько
// сообщений ушло в тему и что в них. Остальное поведение — стендовое.
type countedAPI struct {
	stubAPI

	mu   sync.Mutex
	sent []string

	// limited — отправку начнёт отбивать платформа, когда `budget` кончится.
	limited bool
	budget  int
}

// SendMessage запоминает текст и отдаёт очередной id сообщения; кончился предел — отказ
// платформы.
func (a *countedAPI) SendMessage(_ context.Context, _ int64, text string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.limited {
		if a.budget == 0 {
			return 0, botapi.Refuse(protocol.CodeTelegramRefused, "Bad Request: not enough rights to send messages")
		}

		a.budget--
	}

	a.sent = append(a.sent, text)

	return len(a.sent), nil
}

// refuseAfter даёт отправить ещё столько сообщений, а дальше отправку отбивает платформа.
func (a *countedAPI) refuseAfter(more int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.limited = true
	a.budget = more
}

// count отдаёт число отправленных сообщений.
func (a *countedAPI) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.sent)
}

// texts отдаёт сообщения, отправленные с названной позиции.
func (a *countedAPI) texts(from int) []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.sent[from:]...)
}

// refusingAPI — поддельный Bot API, который отправляет разрешённое тестом число сообщений, а
// дальше отказывает сетью: так выглядит канал, умерший посреди вопросов хука.
type refusingAPI struct {
	stubAPI

	mu    sync.Mutex
	limit int
	sent  int
}

// SendMessage отправляет сообщение, пока разрешено, и дальше отказывает.
func (a *refusingAPI) SendMessage(_ context.Context, _ int64, _ string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.sent >= a.limit {
		return 0, botapi.Refuse(protocol.CodeNetwork, "sendMessage: сеть до Bot API не отвечает")
	}

	a.sent++

	return a.sent, nil
}

// allowMore разрешает ещё столько отправок сверх уже сделанных.
func (a *refusingAPI) allowMore(more int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.limit = a.sent + more
}

// decisionReply — решение хука, как его читает клиент.
type decisionReply struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

// askPayload отдаёт снимок как он записан, его сессию и его вопросы: с ними сверяется и отправка, и
// разбор. Читается JSON без типов, чтобы тест видел ровно те имена полей, что лежат в снимке.
func askPayload(t *testing.T) (string, string, []map[string]any) {
	t.Helper()

	body, err := os.ReadFile(askFixture)
	if err != nil {
		t.Fatalf("снимок хука не прочитан: %v", err)
	}

	var raw map[string]any

	err = json.Unmarshal(body, &raw)
	if err != nil {
		t.Fatalf("снимок хука не разобран: %v", err)
	}

	session, _ := raw["session_id"].(string)
	if session == "" {
		t.Fatal("в снимке нет `session_id`: вопрос некому адресовать")
	}

	toolInput, _ := raw["tool_input"].(map[string]any)
	entries, _ := toolInput["questions"].([]any)

	questions := make([]map[string]any, 0, len(entries))

	for at, entry := range entries {
		question, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("вопрос %d снимка не объект: %+v", at+1, entry)
		}

		questions = append(questions, question)
	}

	if len(questions) < 2 {
		t.Fatalf("в снимке %d вопросов: хуку нужен хотя бы один вопрос на отправку и второй, "+
			"чтобы было видно, что отправка идёт по разу на вопрос", len(questions))
	}

	return string(body), session, questions
}

// option — вариант ответа из снимка: подпись и пояснение, как их прочтёт владелец.
func option(t *testing.T, question map[string]any, at int) (string, string) {
	t.Helper()

	options, ok := question["options"].([]any)
	if !ok || len(options) <= at {
		t.Fatalf("в снимке нет варианта %d: %+v", at, question)
	}

	item, ok := options[at].(map[string]any)
	if !ok {
		t.Fatalf("вариант %d снимка не объект: %+v", at, options[at])
	}

	label, _ := item["label"].(string)
	description, _ := item["description"].(string)

	return label, description
}

// questionText — текст вопроса, как его собирает подкоманда: заголовок, вопрос и пометка «можно
// несколько». Собирается из тех же полей снимка, а не из литерала.
func questionText(t *testing.T, question map[string]any) string {
	t.Helper()

	header, _ := question["header"].(string)
	body, _ := question["question"].(string)
	multi, _ := question["multiSelect"].(bool)

	if header == "" || body == "" {
		t.Fatalf("в снимке нет заголовка или текста вопроса: %+v", question)
	}

	text := header + ": " + body
	if multi {
		text += " (можно несколько)"
	}

	return text
}

// TestHookAskSendsQuestionPerQuestion: хук вопроса с вариантами отдаёт вопрос в тему и решение
// вместо вызова инструмента. Отправка идёт по разу на вопрос: номер варианта владелец набирает по
// одному, и общая нумерация на несколько вопросов сбила бы его.
//
// Параллельным этот тест быть не может: подкоманда читает os.Stdin и печатает в os.Stdout, а стенд
// подменяет их своими трубами.
//
//nolint:paralleltest // стенд подменяет os.Stdin и os.Stdout: двум тестам сразу их не поделить
func TestHookAskSendsQuestionPerQuestion(t *testing.T) {
	api := &countedAPI{} //nolint:exhaustruct // поддельный Bot API стенда, счёт ведёт себя сам

	payload, fixtureSession, questions := askPayload(t)

	cfgPath, stop := serviceFor(t, api, fixtureSession)
	defer stop()

	_ = run(t, 0, "away", "on", "вопросы", "--session", fixtureSession, "--config", cfgPath, "--json")

	sent := api.count()

	out, failures := runIO(t, 0, payload, "hook", "ask", "--config", cfgPath)

	if failures != "" {
		t.Fatalf("успешный вход сказал в ошибки: %q", failures)
	}

	checkDecision(t, out)
	checkQuestions(t, api.texts(sent), questions)
	checkAskedReason(t, out, questions)
}

// checkAskedReason сверяет причину удачного решения: текст хода в режиме тоже уходит в тему
// репликой, поэтому причина велит не повторять в нём вопрос и говорит нейтрально, что ответ придёт
// новым сообщением. Самого вопроса в причине нет: модели нечего переписывать в ход.
func checkAskedReason(t *testing.T, out string, questions []map[string]any) {
	t.Helper()

	var decision decisionReply

	decode(t, out, &decision)

	reason := decision.HookSpecificOutput.PermissionDecisionReason

	for _, want := range []string{
		"Текст хода тоже уходит в эту тему, поэтому вопрос и варианты в нём не повторяй",
		"Ответ придёт новым сообщением — сейчас ждать его не нужно",
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("в причине решения нет %q: %q", want, reason)
		}
	}

	for _, question := range questions {
		text := questionText(t, question)
		if strings.Contains(reason, text) {
			t.Fatalf("причина решения пересказывает вопрос %q: %q", text, reason)
		}
	}
}

// TestHookAskLeavesDialogWithoutService: службы нет — режим живёт в ней, и знать его неоткуда.
// Значит владелец у компьютера: вопрос остаётся в терминале, вызов инструмента не отбирается, а
// строка говорит, почему в тему он не ушёл.
//
// Параллельным этот тест быть не может: подкоманда читает os.Stdin и печатает в os.Stdout, а стенд
// подменяет их своими трубами.
//
//nolint:paralleltest // стенд подменяет os.Stdin и os.Stdout: двум тестам сразу их не поделить
func TestHookAskLeavesDialogWithoutService(t *testing.T) {
	api := &countedAPI{} //nolint:exhaustruct // поддельный Bot API стенда, счёт ведёт себя сам

	payload, fixtureSession, _ := askPayload(t)

	cfgPath, stop := serviceFor(t, api, fixtureSession)

	_ = run(t, 0, "away", "on", "вопросы", "--session", fixtureSession, "--config", cfgPath, "--json")

	stop()

	sent := api.count()

	out, failures := runIO(t, 0, payload, "hook", "ask", "--config", cfgPath)

	if out != "" {
		t.Fatalf("без службы напечатано решение, и диалог отобран: %q", out)
	}

	for _, want := range []string{"службы моста нет", "вопрос остался в терминале"} {
		if !strings.Contains(failures, want) {
			t.Fatalf("отказ не назвал %q: %q", want, failures)
		}
	}

	if api.count() != sent {
		t.Fatalf("без службы в тему ушло %d сообщений", api.count()-sent)
	}
}

// TestHookAskLeavesDialogWithoutConfig: настроек нет — службы не найти, а режим живёт в ней: вопрос
// тоже остаётся в терминале, и строка называет причину.
//
// Параллельным этот тест быть не может: он меняет окружение процесса, а подкоманда читает os.Stdin
// и печатает в os.Stdout, которые стенд подменяет своими трубами.
func TestHookAskLeavesDialogWithoutConfig(t *testing.T) {
	payload, _, _ := askPayload(t)

	t.Setenv("DM_TELEGRAM_CONFIG", "")

	out, failures := runIO(t, 0, payload, "hook", "ask")

	if out != "" {
		t.Fatalf("без настроек напечатано решение, и диалог отобран: %q", out)
	}

	for _, want := range []string{"путь к настройкам не назван", "вопрос остался в терминале"} {
		if !strings.Contains(failures, want) {
			t.Fatalf("отказ не назвал %q: %q", want, failures)
		}
	}
}

// TestHookAskLeavesDialogWhenModeOff: режим у сессии снят — владелец у компьютера, и диалог вопроса
// остаётся в терминале: в тему не уходит ничего, подкоманда молчит и выходит нулём.
//
// Параллельным этот тест быть не может: подкоманда читает os.Stdin и печатает в os.Stdout, а стенд
// подменяет их своими трубами.
//
//nolint:paralleltest // стенд подменяет os.Stdin и os.Stdout: двум тестам сразу их не поделить
func TestHookAskLeavesDialogWhenModeOff(t *testing.T) {
	api := &countedAPI{} //nolint:exhaustruct // поддельный Bot API стенда, счёт ведёт себя сам

	payload, fixtureSession, _ := askPayload(t)

	cfgPath, stop := serviceFor(t, api, fixtureSession)
	defer stop()

	_ = run(t, 0, "away", "on", "вопросы", "--session", fixtureSession, "--config", cfgPath, "--json")
	_ = run(t, 0, "away", "off", "--session", fixtureSession, "--config", cfgPath, "--json")

	sent := api.count()

	out, failures := runIO(t, 0, payload, "hook", "ask", "--config", cfgPath)

	if out != "" || failures != "" {
		t.Fatalf("при снятом режиме вход сказал лишнее: выдача %q, ошибки %q", out, failures)
	}

	if api.count() != sent {
		t.Fatalf("при снятом режиме в тему ушло %d сообщений", api.count()-sent)
	}
}

// TestHookAskBlocksWhenQuestionLost: Telegram отказал посреди вопроса — это не «владелец у
// компьютера», а беда канала. Вызов инструмента отбивается кодом 2, решения на выходе нет, а
// причина называет отказ и говорит задать вопрос текстом хода: диалог в терминале, где владельца
// нет, потерял бы вопрос молча. Ушла часть вопросов — тот же отказ, но повторять велено только
// оставшиеся: открыв диалог целиком, владелец получил бы ушедшие вопросы дважды.
//
// Параллельным этот тест быть не может: подкоманда читает os.Stdin и печатает в os.Stdout, а стенд
// подменяет их своими трубами.
//
//nolint:paralleltest // стенд подменяет os.Stdin и os.Stdout: двум тестам сразу их не поделить
func TestHookAskBlocksWhenQuestionLost(t *testing.T) {
	api := &refusingAPI{} //nolint:exhaustruct // поддельный Bot API стенда, счёт ведёт себя сам

	payload, fixtureSession, _ := askPayload(t)

	cfgPath, stop := serviceFor(t, api, fixtureSession)
	defer stop()

	// Включение режима ставит в тему закреплённую строку: ей отправка разрешена.
	api.allowMore(1)

	_ = run(t, 0, "away", "on", "вопросы", "--session", fixtureSession, "--config", cfgPath, "--json")

	for _, step := range []struct {
		allow int
		want  string
	}{
		{allow: 0, want: "вопрос владельцу не отправлен, задай его текстом хода"},
		{allow: 1, want: "часть вопросов не ушла, задай их текстом хода"},
	} {
		api.allowMore(step.allow)

		out, failures := runIO(t, 2, payload, "hook", "ask", "--config", cfgPath)

		if out != "" {
			t.Fatalf("решение напечатано, хотя вопрос в тему не ушёл: %q", out)
		}

		for _, want := range []string{"вопрос в тему не ушёл", string(protocol.CodeNetwork), step.want} {
			if !strings.Contains(failures, want) {
				t.Fatalf("отказ не назвал %q: %q", want, failures)
			}
		}
	}
}

// TestHookAskRefusesBadPayload: негодная нагрузка — не беда канала, а поломка входа: диалог не
// отбираем, но строкой говорим, что вопрос в тему не ушёл. Сессии в этих нагрузках или нет, или у
// неё нет режима: владелец у компьютера, и диалог он увидит.
//
// Параллельным этот тест быть не может: подкоманда читает os.Stdin и печатает в os.Stdout, а стенд
// подменяет их своими трубами.
//
//nolint:paralleltest // стенд подменяет os.Stdin и os.Stdout: двум тестам сразу их не поделить
func TestHookAskRefusesBadPayload(t *testing.T) {
	api := &countedAPI{} //nolint:exhaustruct // поддельный Bot API стенда, счёт ведёт себя сам

	_, fixtureSession, _ := askPayload(t)

	cfgPath, stop := serviceFor(t, api, fixtureSession)
	defer stop()

	_ = run(t, 0, "away", "on", "вопросы", "--session", fixtureSession, "--config", cfgPath, "--json")

	sent := api.count()

	for name, payload := range badPayloads {
		out, failures := runIO(t, 0, payload, "hook", "ask", "--config", cfgPath)

		if out != "" {
			t.Fatalf("%s: решение напечатано на негодной нагрузке: %q", name, out)
		}

		for _, want := range []string{"нагрузка хука не разобрана", "вопрос остался в терминале"} {
			if !strings.Contains(failures, want) {
				t.Fatalf("%s: отказ не назвал %q: %q", name, want, failures)
			}
		}
	}

	if api.count() != sent {
		t.Fatalf("на негодной нагрузке в тему ушло %d сообщений", api.count()-sent)
	}
}

// TestHookAskBlocksBadQuestionsInMode: вопросы негодные, а сессия читается и у неё включён режим —
// владельца у терминала нет, и диалог открылся бы в пустоту. Вызов инструмента блокируется с
// причиной, и модель задаёт вопрос текстом хода. Режим снят — владелец у компьютера, и диалог с
// негодными вопросами остаётся в терминале.
//
// Параллельным этот тест быть не может: подкоманда читает os.Stdin и печатает в os.Stdout, а стенд
// подменяет их своими трубами.
//
//nolint:paralleltest // стенд подменяет os.Stdin и os.Stdout: двум тестам сразу их не поделить
func TestHookAskBlocksBadQuestionsInMode(t *testing.T) {
	api := &countedAPI{} //nolint:exhaustruct // поддельный Bot API стенда, счёт ведёт себя сам

	_, fixtureSession, _ := askPayload(t)

	cfgPath, stop := serviceFor(t, api, fixtureSession)
	defer stop()

	_ = run(t, 0, "away", "on", "вопросы", "--session", fixtureSession, "--config", cfgPath, "--json")

	sent := api.count()

	for name, payload := range badQuestions(fixtureSession) {
		out, failures := runIO(t, hookBlocked, payload, "hook", "ask", "--config", cfgPath)

		if out != "" {
			t.Fatalf("%s: решение напечатано на негодной нагрузке: %q", name, out)
		}

		for _, want := range []string{"нагрузка хука не разобрана", "включён режим", "задай его текстом хода"} {
			if !strings.Contains(failures, want) {
				t.Fatalf("%s: отказ не назвал %q: %q", name, want, failures)
			}
		}
	}

	_ = run(t, 0, "away", "off", "--session", fixtureSession, "--config", cfgPath, "--json")

	for name, payload := range badQuestions(fixtureSession) {
		out, failures := runIO(t, 0, payload, "hook", "ask", "--config", cfgPath)

		if out != "" {
			t.Fatalf("%s: при снятом режиме решение напечатано, и диалог отобран: %q", name, out)
		}

		for _, want := range []string{"нагрузка хука не разобрана", "вопрос остался в терминале"} {
			if !strings.Contains(failures, want) {
				t.Fatalf("%s: строка не назвала %q: %q", name, want, failures)
			}
		}
	}

	if api.count() != sent {
		t.Fatalf("на негодной нагрузке в тему ушло %d сообщений", api.count()-sent)
	}
}

// TestHookAskLeavesDialogForBadQuestionsWithoutService: вопросы негодные, а службы нет — режим
// живёт в ней, и узнать его неоткуда: диалог остаётся в терминале, как и без негодной нагрузки, а
// строка называет и нагрузку, и то, почему режим не узнан.
//
// Параллельным этот тест быть не может: подкоманда читает os.Stdin и печатает в os.Stdout, а стенд
// подменяет их своими трубами.
//
//nolint:paralleltest // стенд подменяет os.Stdin и os.Stdout: двум тестам сразу их не поделить
func TestHookAskLeavesDialogForBadQuestionsWithoutService(t *testing.T) {
	api := &countedAPI{} //nolint:exhaustruct // поддельный Bot API стенда, счёт ведёт себя сам

	_, fixtureSession, _ := askPayload(t)

	cfgPath, stop := serviceFor(t, api, fixtureSession)

	_ = run(t, 0, "away", "on", "вопросы", "--session", fixtureSession, "--config", cfgPath, "--json")

	stop()

	for name, payload := range badQuestions(fixtureSession) {
		out, failures := runIO(t, 0, payload, "hook", "ask", "--config", cfgPath)

		if out != "" {
			t.Fatalf("%s: без службы напечатано решение, и диалог отобран: %q", name, out)
		}

		for _, want := range []string{
			"нагрузка хука не разобрана", "режим сессии не узнан", "службы моста нет", "вопрос остался в терминале",
		} {
			if !strings.Contains(failures, want) {
				t.Fatalf("%s: строка не назвала %q: %q", name, want, failures)
			}
		}
	}
}

// TestHookAskBlocksWhenServiceRefuses: служба жива, режим включён, а вопрос отбила платформа — это
// беда канала, а не владелец у компьютера. Вызов инструмента блокируется: диалог открылся бы там,
// где владельца нет, а причина говорит модели задать вопрос текстом хода.
//
// Параллельным этот тест быть не может: подкоманда читает os.Stdin и печатает в os.Stdout, а стенд
// подменяет их своими трубами.
//
//nolint:paralleltest // стенд подменяет os.Stdin и os.Stdout: двум тестам сразу их не поделить
func TestHookAskBlocksWhenServiceRefuses(t *testing.T) {
	api := &countedAPI{} //nolint:exhaustruct // поддельный Bot API стенда, счёт ведёт себя сам

	payload, fixtureSession, _ := askPayload(t)

	cfgPath, stop := serviceFor(t, api, fixtureSession)
	defer stop()

	_ = run(t, 0, "away", "on", "вопросы", "--session", fixtureSession, "--config", cfgPath, "--json")

	sent := api.count()
	api.refuseAfter(0)

	out, failures := runIO(t, hookBlocked, payload, "hook", "ask", "--config", cfgPath)

	if out != "" {
		t.Fatalf("на отказе службы напечатано решение «вопрос в теме»: %q", out)
	}

	for _, want := range []string{
		"вопрос в тему не ушёл",
		string(protocol.CodeTelegramRefused),
		"вопрос владельцу не отправлен, задай его текстом хода",
	} {
		if !strings.Contains(failures, want) {
			t.Fatalf("отказ не назвал %q: %q", want, failures)
		}
	}

	if api.count() != sent {
		t.Fatalf("на отказе службы в теме %d новых сообщений", api.count()-sent)
	}
}

// TestHookAskBlocksPartialSend: первый вопрос ушёл в тему, второй отбит. Открыв диалог по всем
// вопросам, владелец получил бы первый дважды — в теме и в терминале, — поэтому вызов
// блокируется, а причина говорит повторить текстом только то, что не ушло.
//
// Параллельным этот тест быть не может: подкоманда читает os.Stdin и печатает в os.Stdout, а стенд
// подменяет их своими трубами.
//
//nolint:paralleltest // стенд подменяет os.Stdin и os.Stdout: двум тестам сразу их не поделить
func TestHookAskBlocksPartialSend(t *testing.T) {
	api := &countedAPI{} //nolint:exhaustruct // поддельный Bot API стенда, счёт ведёт себя сам

	payload, fixtureSession, questions := askPayload(t)

	cfgPath, stop := serviceFor(t, api, fixtureSession)
	defer stop()

	_ = run(t, 0, "away", "on", "вопросы", "--session", fixtureSession, "--config", cfgPath, "--json")

	sent := api.count()
	api.refuseAfter(1)

	out, failures := runIO(t, hookBlocked, payload, "hook", "ask", "--config", cfgPath)

	if out != "" {
		t.Fatalf("на частичной отправке напечатано решение «вопрос в теме»: %q", out)
	}

	for _, want := range []string{"вопрос в тему не ушёл", "часть вопросов не ушла, задай их текстом хода"} {
		if !strings.Contains(failures, want) {
			t.Fatalf("отказ не назвал %q: %q", want, failures)
		}
	}

	messages := api.texts(sent)
	if len(messages) != 1 || !strings.Contains(messages[0], questionText(t, questions[0])) {
		t.Fatalf("в теме не ровно первый вопрос: %q", messages)
	}
}

// badQuestions — нагрузки с читаемой сессией, но вопросами, которыми владельца не спросить: по
// сессии вход хука спрашивает у службы режим.
func badQuestions(session string) map[string]string {
	head := `{"session_id":"` + session + `","tool_input":`

	return map[string]string{
		"нет вопросов":       head + `{"questions":[]}}`,
		"вопросы не списком": head + `{"questions":"как быть?"}}`,
		"вопрос без текста":  head + `{"questions":[{"question":" "}]}}`,
		"вариант без подписи": head + `{"questions":` +
			`[{"question":"как?","options":[{"label":"","description":"пусто"}]}]}}`,
	}
}

// badPayloads — нагрузки, на которых вопрос отправлять нечем: разбор и проверка обязанных полей.
var badPayloads = map[string]string{
	"не JSON":           `{`,
	"пусто":             "",
	"нет сессии":        `{"tool_input":{"questions":[{"question":"как быть?"}]}}`,
	"нет вопросов":      `{"session_id":"ses-thin","tool_input":{"questions":[]}}`,
	"вопрос без текста": `{"session_id":"ses-thin","tool_input":{"questions":[{"question":" "}]}}`,
	"вариант без подписи": `{"session_id":"ses-thin","tool_input":{"questions":` +
		`[{"question":"как?","options":[{"label":"","description":"пусто"}]}]}}`,
}

// checkDecision сверяет решение хука: событие, отказ вызова инструмента и причину с числом
// сообщений.
func checkDecision(t *testing.T, out string) {
	t.Helper()

	if !strings.HasPrefix(strings.TrimSpace(out), `{"hookSpecificOutput":`) {
		t.Fatalf("решение не того вида: %q", out)
	}

	var decision decisionReply

	decode(t, out, &decision)

	if decision.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("событие хука не названо: %+v", decision)
	}

	if decision.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("вызов инструмента не отменён: %+v", decision)
	}

	reason := decision.HookSpecificOutput.PermissionDecisionReason
	if reason == "" {
		t.Fatalf("решение без причины: %+v", decision)
	}
}

// checkQuestions сверяет сообщения темы с вопросами снимка: на каждый вопрос своё сообщение, а
// варианты пронумеровала служба, а не звонящий.
func checkQuestions(t *testing.T, messages []string, questions []map[string]any) {
	t.Helper()

	if len(messages) != len(questions) {
		t.Fatalf("в тему ушло %d сообщений на %d вопросов: %q", len(messages), len(questions), messages)
	}

	for at, question := range questions {
		text := questionText(t, question)

		if !strings.Contains(messages[at], text) {
			t.Fatalf("в сообщении %d нет вопроса %q: %q", at+1, text, messages[at])
		}

		label, description := option(t, question, 0)
		numbered := fmt.Sprintf("1) %s — %s", label, description)

		if !strings.Contains(messages[at], numbered) {
			t.Fatalf("варианты вопроса %d не пронумерованы службой (%q): %q", at+1, numbered, messages[at])
		}
	}
}
