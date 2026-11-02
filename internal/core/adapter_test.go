package core_test

import (
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestAdapterIsEqualToMod: плагин OpenCode — такой же держатель соединения сессии, как мод. Всё,
// что умеет мод, работает для него без оговорок: режим, поток реплик, доставка, обе отметки и
// исполнение команды. Разницу владелец чувствовать не должен ни в чём.
//
//nolint:cyclop // равенство клиентов проверяется одним путём целиком: шаг за шагом
func TestAdapterIsEqualToMod(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	adapter, hello := rig.hello("s1", protocol.RoleSession, protocol.KindAdapter)
	if !hello.OK {
		t.Fatalf("адаптер соединение сессии не получил: %+v", hello.Error)
	}

	on := rig.req(protocol.OpModeOn)
	on.Name = topicAlpha

	topic := rig.do(adapter, on)
	if !topic.OK || topic.TopicName != topicAlpha {
		t.Fatalf("адаптер режим не включил: %+v", topic)
	}

	// Поток реплик агента.
	stream := rig.req(protocol.OpAgentMessage)
	stream.Text = "реплика адаптера"

	if said := rig.do(adapter, stream); !said.OK || said.MessageID == 0 {
		t.Fatalf("адаптер поток не ведёт: %+v", said)
	}

	// Границы хода и вопрос владельцу.
	if resp := rig.do(adapter, rig.req(protocol.OpTurnStart)); !resp.OK {
		t.Fatalf("адаптеру начало хода недоступно: %+v", resp.Error)
	}

	if resp := rig.do(adapter, rig.req(protocol.OpTurnEnd)); !resp.OK {
		t.Fatalf("адаптеру конец хода недоступен: %+v", resp.Error)
	}

	// Доставка команды и обе отметки.
	ownerID := rig.ownerMessage(topic.TopicID, "/compact")

	got := rig.waitDelivery(adapter)
	if got.Kind != protocol.DeliveryClientCmd {
		t.Fatalf("адаптер команду клиента не получил: %+v", got)
	}

	if rig.api.mark(ownerID) != botapi.MarkReceived {
		t.Fatalf("первая отметка не поставлена: %q", rig.api.mark(ownerID))
	}

	rig.ack(adapter, got.DeliveryID, protocol.AckAccepted, "")

	if rig.api.mark(ownerID) != botapi.MarkAccepted {
		t.Fatalf("вторая отметка не поставлена: %q", rig.api.mark(ownerID))
	}
}

// TestModeStatusFeedsOpenCodeLine: строка внизу окна OpenCode с именем темы живёт запросом
// состояния режима — не чтением состояния моста. Запрос остаётся требованием владельца, и
// отвечает он и держателю сессии, и короткому вызову.
func TestModeStatusFeedsOpenCodeLine(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	adapter, _ := rig.hello("s1", protocol.RoleSession, protocol.KindAdapter)

	on := rig.req(protocol.OpModeOn)
	on.Name = topicInventory
	rig.do(adapter, on)

	// Сам плагин спрашивает состояние своим же соединением.
	own := rig.do(adapter, rig.req(protocol.OpModeStatus))
	if own.Mode != protocol.ModeOn || own.TopicName != topicInventory {
		t.Fatalf("плагин не видит имени темы: %+v", own)
	}

	// И коротким вызовом — так же, называя сессию.
	tool := rig.tool("s1")
	status := rig.req(protocol.OpModeStatus)
	status.Session = "s1"

	outside := rig.do(tool, status)
	if outside.Mode != protocol.ModeOn || outside.TopicName != topicInventory {
		t.Fatalf("коротким вызовом имя темы не узнать: %+v", outside)
	}
}
