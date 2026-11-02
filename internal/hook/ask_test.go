package hook_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/hook"
)

// askFixture — снимок вызова `AskUserQuestion`, снятый с живого клиента: форма входа хука берётся
// оттуда, а не литералом, и её переименование у клиента валит этот прогон.
const askFixture = "testdata/PreToolUse.ask-user-question.json"

// fixture — снимок как он записан: вопросы читаются без типов, чтобы сверка видела ровно те имена
// полей, что лежат в файле, и падала, если поля в снимке нет.
type fixture struct {
	session   string
	questions []map[string]any
}

// readFixture читает снимок и отдаёт его сессию и вопросы.
func readFixture(t *testing.T) fixture {
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
		t.Fatal("в снимке нет `session_id`")
	}

	toolInput, _ := raw["tool_input"].(map[string]any)
	entries, _ := toolInput["questions"].([]any)

	if len(entries) == 0 {
		t.Fatal("в снимке нет вопросов")
	}

	questions := make([]map[string]any, 0, len(entries))

	for at, entry := range entries {
		question, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("вопрос %d снимка не объект: %+v", at+1, entry)
		}

		questions = append(questions, question)
	}

	return fixture{session: session, questions: questions}
}

// options отдаёт варианты вопроса снимка.
func options(t *testing.T, question map[string]any) []map[string]any {
	t.Helper()

	entries, _ := question["options"].([]any)
	if len(entries) == 0 {
		t.Fatalf("у вопроса снимка нет вариантов: %+v", question)
	}

	found := make([]map[string]any, 0, len(entries))

	for at, entry := range entries {
		option, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("вариант %d снимка не объект: %+v", at+1, entry)
		}

		found = append(found, option)
	}

	return found
}

// TestParseReadsQuestions: нагрузка разбирается в вопросы с вариантами, и каждый разобранный вопрос
// совпадает с тем, что записано в снимке: переименованное поле клиента валит сверку, а не теряет
// варианты молча.
func TestParseReadsQuestions(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(askFixture)
	if err != nil {
		t.Fatalf("снимок хука не прочитан: %v", err)
	}

	payload, err := hook.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("снимок не разобран: %v", err)
	}

	fixed := readFixture(t)

	if payload.SessionID != fixed.session {
		t.Fatalf("сессия не та: %q вместо %q", payload.SessionID, fixed.session)
	}

	questions := payload.Questions()
	if len(questions) != len(fixed.questions) {
		t.Fatalf("вопросов %d вместо %d", len(questions), len(fixed.questions))
	}

	for at, raw := range fixed.questions {
		checkQuestion(t, questions[at], raw, at)
	}
}

// checkQuestion сверяет разобранный вопрос с записанным в снимке: поля, текст и варианты.
func checkQuestion(t *testing.T, question hook.Question, raw map[string]any, at int) {
	t.Helper()

	header, _ := raw["header"].(string)
	text, _ := raw["question"].(string)
	multi, _ := raw["multiSelect"].(bool)

	if header == "" || text == "" {
		t.Fatalf("в снимке нет заголовка или текста вопроса %d: %+v", at+1, raw)
	}

	if question.Header != header || question.Question != text || question.MultiSelect != multi {
		t.Fatalf("вопрос %d разобран не по снимку: %+v", at+1, question)
	}

	want := header + ": " + text
	if multi {
		want += " (можно несколько)"
	}

	if question.Text() != want {
		t.Fatalf("текст вопроса %d не тот: %q", at+1, question.Text())
	}

	checkOptions(t, question, raw, at)
}

// checkOptions сверяет варианты: их столько же, сколько в снимке, и подписи с пояснениями те же.
func checkOptions(t *testing.T, question hook.Question, raw map[string]any, at int) {
	t.Helper()

	want := options(t, raw)
	choices := question.Choices()

	if len(choices) != len(want) {
		t.Fatalf("вариантов %d вместо %d у вопроса %d", len(choices), len(want), at+1)
	}

	for idx, option := range want {
		label, _ := option["label"].(string)
		description, _ := option["description"].(string)

		if label == "" {
			t.Fatalf("у варианта %d вопроса %d снимка нет подписи: %+v", idx+1, at+1, option)
		}

		if choices[idx].Key != label || choices[idx].Text != description {
			t.Fatalf("вариант %d вопроса %d разобран не по снимку: %+v", idx+1, at+1, choices[idx])
		}
	}
}

// TestParseRefusesBadPayload: негодная нагрузка — свой отказ с причиной, а не пустой вопрос: без
// сессии вопрос не адресован, без текста это не вопрос, вариант без подписи владелец не прочтёт.
func TestParseRefusesBadPayload(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"не JSON":            `{`,
		"пусто":              "",
		"нет сессии":         `{"tool_input":{"questions":[{"question":"как быть?"}]}}`,
		"сессия из пробелов": `{"session_id":" ","tool_input":{"questions":[{"question":"как?"}]}}`,
		"нет вопросов":       `{"session_id":"s","tool_input":{"questions":[]}}`,
		"вопрос без текста":  `{"session_id":"s","tool_input":{"questions":[{"question":" "}]}}`,
		"вариант без подписи": `{"session_id":"s","tool_input":{"questions":` +
			`[{"question":"как?","options":[{"label":" "}]}]}}`,
	}

	for name, payload := range cases {
		_, err := hook.Parse(strings.NewReader(payload))
		if !errors.Is(err, hook.ErrBadPayload) {
			t.Fatalf("%s: отказ не назван (%v)", name, err)
		}
	}
}

// TestParseNamesSessionOfBadQuestions: сессия читается и тогда, когда вопросы негодные, — даже
// когда вход инструмента другого вида, чем ждёт разбор. По ней вход хука спрашивает у службы
// режим. Нагрузка без читаемой сессии сессию не называет: спрашивать службу не о ком.
func TestParseNamesSessionOfBadQuestions(t *testing.T) {
	t.Parallel()

	withSession := map[string]string{
		"нет вопросов":       `{"session_id":"s","tool_input":{"questions":[]}}`,
		"нет входа":          `{"session_id":"s"}`,
		"вопросы не списком": `{"session_id":"s","tool_input":{"questions":"как быть?"}}`,
		"вопрос без текста":  `{"session_id":"s","tool_input":{"questions":[{"question":" "}]}}`,
		"вариант без подписи": `{"session_id":"s","tool_input":{"questions":` +
			`[{"question":"как?","options":[{"label":" "}]}]}}`,
	}

	for name, payload := range withSession {
		_, err := hook.Parse(strings.NewReader(payload))

		var bad *hook.QuestionsError
		if !errors.As(err, &bad) || bad.Session != "s" || !errors.Is(err, hook.ErrBadPayload) {
			t.Fatalf("%s: отказ не назвал сессию негодных вопросов: %v", name, err)
		}
	}

	withoutSession := map[string]string{
		"не JSON":            `{`,
		"нет сессии":         `{"tool_input":{"questions":[{"question":"как быть?"}]}}`,
		"сессия из пробелов": `{"session_id":" ","tool_input":{"questions":[]}}`,
		"сессия не строкой":  `{"session_id":7,"tool_input":{"questions":[]}}`,
	}

	for name, payload := range withoutSession {
		_, err := hook.Parse(strings.NewReader(payload))

		var bad *hook.QuestionsError
		if errors.As(err, &bad) || !errors.Is(err, hook.ErrBadPayload) {
			t.Fatalf("%s: отказ без читаемой сессии назвал сессию: %v", name, err)
		}
	}
}

// TestDenyShape: решение отдаётся той формой, которую читает клиент, — `PreToolUse`, `deny` и
// причина словами.
func TestDenyShape(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	err := hook.Deny("вопрос отправлен").Write(&out)
	if err != nil {
		t.Fatalf("решение не напечатано: %v", err)
	}

	var decision struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}

	err = json.Unmarshal(out.Bytes(), &decision)
	if err != nil {
		t.Fatalf("решение не разобрано (%q): %v", out.String(), err)
	}

	if decision.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("событие не названо: %+v", decision)
	}

	if decision.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("вызов инструмента не отменён: %+v", decision)
	}

	if decision.HookSpecificOutput.PermissionDecisionReason != "вопрос отправлен" {
		t.Fatalf("причина не та: %+v", decision)
	}

	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("решение напечатано не одной строкой: %q", out.String())
	}
}
