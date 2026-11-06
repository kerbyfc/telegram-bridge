package link_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/link"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// reattach проводит приветствие вернувшейся службы и дожидается события готовности.
func reattach(t *testing.T, srv *service, mod *probe) {
	t.Helper()

	hello := srv.take(t)
	if hello.Op != protocol.OpHello {
		t.Fatalf("вернувшейся службе первым пришёл кадр `%s`", hello.Op)
	}

	resp := protocol.Ok(hello.RID)
	resp.Mode = protocol.ModeOn
	srv.reply(t, resp)

	ready := mod.event(t)
	if ready.Event != link.EventReady {
		t.Fatalf("соединение не открылось: %+v", ready)
	}
}

// TestAckGivenWithoutLinkReachesNewService: служба ушла на перезапуск, пока мод подавал слово
// владельца. Подтверждение, данное без связи, подкоманда принимает и отдаёт вернувшейся службе
// первым кадром, раньше долгого запроса, — иначе служба подала бы то же слово второй раз.
func TestAckGivenWithoutLinkReachesNewService(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	srv.hand(t, srv.take(t), first, "посмотри на диффе")
	handed(t, mod.event(t))

	srv.vanish(t)

	if lost := mod.event(t); lost.Event != link.EventLost {
		t.Fatalf("о потере соединения сказано не так: %+v", lost)
	}

	status, answer := mod.ack(t, link.Ack{DeliveryID: first, Outcome: protocol.AckAccepted, Detail: ""})
	if status != http.StatusOK || !answer.OK {
		t.Fatalf("подтверждение без связи не принято: %d %+v", status, answer)
	}

	srv.revive(t)
	reattach(t, srv, mod)

	ack := srv.take(t)
	if ack.Op != protocol.OpDeliveryAck || ack.DeliveryID != first || ack.Outcome != protocol.AckAccepted {
		t.Fatalf("вернувшейся службе первым ушло не подтверждение: %+v", ack)
	}

	srv.ok(t, ack)

	wait := srv.take(t)
	if wait.Op != protocol.OpDeliveryWait || wait.After != first {
		t.Fatalf("долгий запрос после подтверждения не тот: %+v", wait)
	}
}

// TestNewServiceWaitsForPendingAck: связь вернулась раньше, чем мод подал слово. Долгого запроса
// подкоманда не шлёт, пока мод не подтвердил выданное прежнему соединению: подтверждение уходит
// новым соединением, и только за ним — долгий запрос.
func TestNewServiceWaitsForPendingAck(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	srv.hand(t, srv.take(t), first, "посмотри на диффе")
	handed(t, mod.event(t))

	srv.vanish(t)
	mod.event(t)
	srv.revive(t)
	reattach(t, srv, mod)

	srv.silent(t)

	answered := make(chan link.Answer, 1)

	go func() {
		_, answer := mod.ack(t, link.Ack{DeliveryID: first, Outcome: protocol.AckAccepted, Detail: ""})
		answered <- answer
	}()

	ack := srv.take(t)
	if ack.Op != protocol.OpDeliveryAck || ack.DeliveryID != first {
		t.Fatalf("подтверждение не ушло новым соединением: %+v", ack)
	}

	srv.ok(t, ack)

	if answer := <-answered; !answer.OK {
		t.Fatalf("подтверждение не принято: %+v", answer)
	}

	if wait := srv.take(t); wait.Op != protocol.OpDeliveryWait || wait.After != first {
		t.Fatalf("долгий запрос после подтверждения не тот: %+v", wait)
	}
}

// TestRepliesWaitForService: реплики агента, пришедшие без связи, подкоманда копит и отдаёт
// вернувшейся службе в порядке прихода.
func TestRepliesWaitForService(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	srv.take(t)

	srv.vanish(t)
	mod.event(t)

	for _, text := range []string{"первая", "вторая"} {
		status, answer := mod.post(t, "/message", link.Message{Text: text})
		if status != http.StatusOK || !answer.OK {
			t.Fatalf("реплика без связи не принята: %d %+v", status, answer)
		}
	}

	srv.revive(t)
	reattach(t, srv, mod)

	var replies []string

	for len(replies) < 2 {
		req := srv.take(t)
		if req.Op == protocol.OpDeliveryWait {
			continue
		}

		if req.Op != protocol.OpAgentMessage {
			t.Fatalf("вернувшейся службе пришёл кадр `%s`", req.Op)
		}

		replies = append(replies, req.Text)
		srv.ok(t, req)
	}

	if replies[0] != "первая" || replies[1] != "вторая" {
		t.Fatalf("реплики отданы не в порядке прихода: %v", replies)
	}

	mod.still(t, quiet)
}

// TestRefusedQueuedReplyIsSaid: реплику из очереди служба отбила не снятым режимом — средство сессии
// узнаёт, что она до темы не дошла. Снятый режим — обычное дело, о нём событий нет.
func TestRefusedQueuedReplyIsSaid(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	srv.take(t)

	srv.vanish(t)
	mod.event(t)

	mod.post(t, "/message", link.Message{Text: "вне режима"})
	mod.post(t, "/message", link.Message{Text: "в пропавшую тему"})

	srv.revive(t)
	reattach(t, srv, mod)

	for answered := 0; answered < 2; {
		req := srv.take(t)
		if req.Op != protocol.OpAgentMessage {
			continue
		}

		answered++

		if req.Text == "вне режима" {
			srv.fail(t, req, protocol.CodeNoMode, "режим снят")

			continue
		}

		srv.fail(t, req, protocol.CodeNoTopic, "тема пропала")
	}

	said := mod.event(t)
	if said.Event != link.EventUndelivered || said.Text == "" {
		t.Fatalf("о непринятой реплике сказано не так: %+v", said)
	}

	mod.still(t, quiet)
}

// TestCrashLoopReachesAbsent: служба падает сразу после приветствия, раз за разом. Такая петля —
// та же пропажа, а не череда возвратов: `absent` приходит в свой срок, а `steady` — нет.
func TestCrashLoopReachesAbsent(t *testing.T) {
	t.Parallel()

	const absent = 10 * retry

	srv := listen(t)
	mod := startWith(t, srv, protocol.ClientClaudeCode, protocol.KindMod, absent)
	attach(t, srv, mod)
	srv.take(t)

	srv.drop(t)

	if lost := mod.event(t); lost.Event != link.EventLost {
		t.Fatalf("о потере сказано не так: %+v", lost)
	}

	deadline := time.Now().Add(patience)

	for {
		if time.Now().After(deadline) {
			t.Fatal("петля падений не дошла до `absent`")
		}

		hello := srv.take(t)
		if hello.Op != protocol.OpHello {
			continue
		}

		srv.ok(t, hello)
		srv.drop(t)

		event := mod.event(t)
		if event.Event == link.EventAbsent {
			break
		}

		if event.Event != link.EventReady {
			t.Fatalf("в петле падений пришло событие `%s`", event.Event)
		}
	}

	mod.unsteady(t, quiet)
}

// TestSteadyAfterLongEnoughLink: соединение, прожившее срок, говорит `steady` — по нему средство
// сессии снимает сказанное о пропаже.
func TestSteadyAfterLongEnoughLink(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	mod.steadied(t)
}

// TestHelloHasDeadline: служба приняла соединение и молчит на приветствие — подкоманда не висит, а
// считает это потерей связи и звонит снова.
func TestHelloHasDeadline(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)

	if hello := srv.take(t); hello.Op != protocol.OpHello {
		t.Fatalf("первым пришёл кадр `%s`", hello.Op)
	}

	if lost := mod.event(t); lost.Event != link.EventLost {
		t.Fatalf("о молчащей службе сказано не так: %+v", lost)
	}

	if again := srv.take(t); again.Op != protocol.OpHello {
		t.Fatalf("после молчания приветствия не повторено: %+v", again)
	}
}
