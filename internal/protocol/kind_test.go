package protocol_test

import (
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestKindOfNamesSessionTool: роль сессии держит мод у Claude Code и адаптер у OpenCode. Перевод
// нужен ровно для приветствия, и клиенты в нём равны: вид отличается именем, а не правами.
func TestKindOfNamesSessionTool(t *testing.T) {
	t.Parallel()

	cases := map[protocol.Client]protocol.Kind{
		protocol.ClientClaudeCode: protocol.KindMod,
		protocol.ClientOpenCode:   protocol.KindAdapter,
	}

	for client, want := range cases {
		kind, known := protocol.KindOf(client)
		if !known || kind != want {
			t.Fatalf("клиент %q назвался видом %q (известен: %v), а ждали %q", client, kind, known, want)
		}
	}

	if len(protocol.Clients()) != len(cases) {
		t.Fatalf("перечисление клиентов — %v, а перевод знает %d", protocol.Clients(), len(cases))
	}
}

// TestKindOfRefusesStranger: незнакомому клиенту вид не подставляется. Сессию держит только мод
// или адаптер, и догадка здесь обернулась бы полуживой сессией, которой нечем подать слово
// владельца.
func TestKindOfRefusesStranger(t *testing.T) {
	t.Parallel()

	kind, known := protocol.KindOf("cursor")
	if known || kind != "" {
		t.Fatalf("незнакомый клиент получил вид %q (известен: %v)", kind, known)
	}
}
