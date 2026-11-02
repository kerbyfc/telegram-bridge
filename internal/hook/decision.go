package hook

import (
	"encoding/json"
	"fmt"
	"io"
)

// Решение хука: значения полей заданы протоколом хуков клиента, и два мерила имён развели бы
// решение и контракт.
const (
	eventPreToolUse = "PreToolUse"
	decisionDeny    = "deny"
)

// Outcome — решение по вызову инструмента, как его читает клиент.
type Outcome struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

// Decision — ответ хуку: решение вместо вызова инструмента.
type Decision struct {
	HookSpecificOutput Outcome `json:"hookSpecificOutput"`
}

// Write печатает решение одной строкой JSON — так его читает клиент.
func (d Decision) Write(w io.Writer) error {
	err := json.NewEncoder(w).Encode(d)
	if err != nil {
		return fmt.Errorf("решение хука не напечатано: %w", err)
	}

	return nil
}

// Deny собирает решение «вызов инструмента не открывается, причина — эта».
func Deny(reason string) Decision {
	return Decision{HookSpecificOutput: Outcome{
		HookEventName:            eventPreToolUse,
		PermissionDecision:       decisionDeny,
		PermissionDecisionReason: reason,
	}}
}
