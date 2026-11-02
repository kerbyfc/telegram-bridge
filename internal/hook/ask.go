// Package hook — вход хука Claude Code, которым второй вход моста, вопрос с вариантами, попадает
// в тему: нагрузка клиента приходит на стандартный вход, решение уходит на стандартный выход.
//
// Разбор нагрузки живёт здесь, а не строкой в настройках хука: форма `tool_input` принадлежит
// клиенту и меняется вместе с ним, а место, где она разобрана, обязано быть под тестами моста.
// Клиент волен добавлять и переименовывать поля нагрузки — читаются только те, что названы ниже.
package hook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// ErrBadPayload — нагрузка клиента не разобрана: отправлять в тему нечего.
var ErrBadPayload = errors.New("нагрузка хука не разобрана")

// Option — вариант ответа, как его объявляет инструмент клиента.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Question — один вопрос инструмента: заголовок, текст, варианты и признак «можно несколько».
type Question struct {
	Header      string   `json:"header"`
	Question    string   `json:"question"`
	MultiSelect bool     `json:"multiSelect"`
	Options     []Option `json:"options"`
}

// Text — текст вопроса, как он уйдёт в тему: заголовок, сам вопрос и пометка о нескольких ответах.
func (q Question) Text() string {
	body := q.Question
	if q.Header != "" {
		body = q.Header + ": " + body
	}

	if q.MultiSelect {
		body += " (можно несколько)"
	}

	return body
}

// Choices — варианты ответа в виде протокола: ключ — подпись, текст — пояснение. Нумерацию ключей
// делает служба, а не звонящий, поэтому подпись едет как есть.
func (q Question) Choices() []protocol.Option {
	choices := make([]protocol.Option, 0, len(q.Options))

	for _, option := range q.Options {
		choices = append(choices, protocol.Option{Key: option.Label, Text: option.Description})
	}

	return choices
}

// Input — вход вызова инструмента: вопросы, которые он собирался задать.
type Input struct {
	Questions []Question `json:"questions"`
}

// Payload — нагрузка хука PreToolUse на AskUserQuestion: сессия и вопросы её инструмента.
type Payload struct {
	SessionID string
	ToolInput Input
}

// Questions отдаёт вопросы нагрузки.
func (p Payload) Questions() []Question {
	return p.ToolInput.Questions
}

// validate проверяет вопросы нагрузки: без вопроса спрашивать нечего, без текста это не вопрос, а
// вариант без подписи владелец не прочтёт.
func (p Payload) validate() error {
	if len(p.ToolInput.Questions) == 0 {
		return fmt.Errorf("%w: нет ни одного вопроса", ErrBadPayload)
	}

	for num, question := range p.ToolInput.Questions {
		if strings.TrimSpace(question.Question) == "" {
			return fmt.Errorf("%w: у вопроса %d нет текста", ErrBadPayload, num+1)
		}

		for idx, option := range question.Options {
			if strings.TrimSpace(option.Label) == "" {
				return fmt.Errorf("%w: у вопроса %d нет подписи у варианта %d", ErrBadPayload, num+1, idx+1)
			}
		}
	}

	return nil
}

// QuestionsError — нагрузка с читаемой сессией, но вопросами, которыми владельца не спросить.
// Сессию отказ несёт затем, чтобы вход хука спросил у службы её режим: при включённом режиме
// диалог в терминале некому увидеть, и отдавать ему негодные вопросы нельзя.
type QuestionsError struct {
	// Session — сессия нагрузки: о ней вход хука и спрашивает службу.
	Session string

	reason error
}

// Error называет, чем негодны вопросы.
func (e *QuestionsError) Error() string {
	return e.reason.Error()
}

// Unwrap отдаёт причину: отказ остаётся отказом `ErrBadPayload`.
func (e *QuestionsError) Unwrap() error {
	return e.reason
}

// envelope — нагрузка до разбора вопросов: сессия читается и тогда, когда вход инструмента
// негодный, — даже другого вида, чем ждёт разбор.
//
//nolint:tagliatelle // имена полей заданы нагрузкой клиента и пишутся как в ней
type envelope struct {
	SessionID string          `json:"session_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// Parse читает нагрузку и проверяет, что из неё есть чем задать вопрос владельцу. Без сессии
// вопрос не адресован — это `ErrBadPayload`. Сессия есть, а вопросы негодные, — это
// `*QuestionsError`: он тоже `ErrBadPayload`, но называет сессию.
func Parse(r io.Reader) (Payload, error) {
	var raw envelope

	err := json.NewDecoder(r).Decode(&raw)
	if err != nil {
		return Payload{}, fmt.Errorf("%w: %w", ErrBadPayload, err)
	}

	if strings.TrimSpace(raw.SessionID) == "" {
		return Payload{}, fmt.Errorf("%w: нет `session_id`", ErrBadPayload)
	}

	payload := Payload{SessionID: raw.SessionID, ToolInput: Input{Questions: nil}}

	// Входа инструмента нет вовсе — вопросов нет: это скажет проверка, а не разбор пустоты.
	if len(raw.ToolInput) > 0 {
		err = json.Unmarshal(raw.ToolInput, &payload.ToolInput)
		if err != nil {
			return Payload{}, &QuestionsError{
				Session: raw.SessionID,
				reason:  fmt.Errorf("%w: `tool_input` не разобран: %w", ErrBadPayload, err),
			}
		}
	}

	err = payload.validate()
	if err != nil {
		return Payload{}, &QuestionsError{Session: raw.SessionID, reason: err}
	}

	return payload, nil
}
