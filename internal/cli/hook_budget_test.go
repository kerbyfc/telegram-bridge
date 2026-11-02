package cli //nolint:testpackage // срок подкоманды неэкспортирован: сверяется с таймаутом хука

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// settingsPath — пример строки хука в настройках клиента: ровно тот, что называет README, и с тем же
// таймаутом. Проверять настоящие настройки владельца тесту нечем — у службы их нет.
const settingsPath = "testdata/settings.json"

// TestHookBudgetFitsTheHookTimeout: срок подкоманды на всю нагрузку обязан быть короче таймаута
// хука в настройках клиента с запасом на запуск. Иначе клиент убьёт подкоманду раньше, чем она
// напечатает решение, а оборванный хук клиент считает некритичной ошибкой и открывает диалог в
// терминале, где владельца нет.
func TestHookBudgetFitsTheHookTimeout(t *testing.T) {
	t.Parallel()

	timeout := hookTimeout(t)

	if hookBudget >= timeout {
		t.Fatalf("срок подкоманды %s не короче таймаута хука %s", hookBudget, timeout)
	}

	if timeout-hookBudget < time.Second {
		t.Fatalf("запас %s меньше секунды: запуск обёртки и бинаря тоже тратит срок", timeout-hookBudget)
	}
}

// hookTimeout читает таймаут хука `AskUserQuestion` из настроек клиента. Нет строки — отказ: сверять
// срок не с чем, и молчаливое «настроек нет» скрыло бы как раз тот случай, ради которого тест есть.
func hookTimeout(t *testing.T) time.Duration {
	t.Helper()

	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("настройки клиента не прочитаны: %v", err)
	}

	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Timeout int `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}

	err = json.Unmarshal(body, &settings)
	if err != nil {
		t.Fatalf("настройки клиента не разобраны: %v", err)
	}

	for _, group := range settings.Hooks["PreToolUse"] {
		if group.Matcher != "AskUserQuestion" {
			continue
		}

		if len(group.Hooks) == 0 || group.Hooks[0].Timeout <= 0 {
			t.Fatalf("у хука `AskUserQuestion` нет таймаута: %+v", group)
		}

		return time.Duration(group.Hooks[0].Timeout) * time.Second
	}

	t.Fatal("в настройках клиента нет хука `AskUserQuestion`")

	return 0
}
