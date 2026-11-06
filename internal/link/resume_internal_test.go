package link

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestPostponedAckWakesResume: подтверждение приёма откладывается гонкой уже после того, как
// вернувшееся соединение начало его ждать (`confirm` застал соединения нет, а `resume` уже идёт).
// Ожидание обязано проснуться и отдать подтверждение службе сразу, а не лежать до следующего
// обрыва.
func TestPostponedAckWakesResume(t *testing.T) {
	t.Parallel()

	near, far := net.Pipe()

	t.Cleanup(func() {
		_ = near.Close()
		_ = far.Close()
	})

	session := New(Options{ //nolint:exhaustruct // ожиданию хватает этих полей
		Session: "s-1", Client: protocol.ClientClaudeCode, Kind: protocol.KindMod,
		Events: io.Discard, Log: io.Discard,
	})

	taken := session.expect("d-1")
	client := newClient(near)

	session.adopt(client)

	resumed := make(chan error, 1)

	go func() { resumed <- session.resume(context.Background(), client) }()

	// Ожидание уже идёт: кадров нет.
	frames := make(chan protocol.Request, 1)

	go func() {
		req, err := protocol.NewReader(far).Read()
		if err == nil {
			frames <- req
		}
	}()

	select {
	case req := <-frames:
		t.Fatalf("до подтверждения ушёл кадр `%s`", req.Op)
	case <-time.After(50 * time.Millisecond):
	}

	session.postpone(Ack{DeliveryID: "d-1", Outcome: protocol.AckAccepted, Detail: ""}, taken)

	answerAck(t, far, frames)
	finished(t, resumed)
}

// answerAck ждёт кадр отложенного подтверждения и отвечает на него удачей.
func answerAck(t *testing.T, far net.Conn, frames chan protocol.Request) {
	t.Helper()

	select {
	case req := <-frames:
		if req.Op != protocol.OpDeliveryAck || req.DeliveryID != "d-1" {
			t.Fatalf("отложенное подтверждение ушло не тем кадром: %+v", req)
		}

		err := protocol.NewWriter(far).Write(protocol.Ok(req.RID))
		if err != nil {
			t.Fatalf("ответ на подтверждение не отправлен: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("отложенное подтверждение лежит: ожидание не проснулось")
	}
}

// finished ждёт конца доведения выданной доставки без ошибки.
func finished(t *testing.T, resumed chan error) {
	t.Helper()

	select {
	case err := <-resumed:
		if err != nil {
			t.Fatalf("доведение выданной доставки кончилось ошибкой: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("доведение выданной доставки не кончилось")
	}
}

// TestQueueKeepsArrivalOrder: реплики, оборванные на лету, возвращаются в очередь в любом порядке,
// а встают по номеру прихода — младшая не обгоняет старшую.
func TestQueueKeepsArrivalOrder(t *testing.T) {
	t.Parallel()

	session := New(Options{ //nolint:exhaustruct // очереди хватает этих полей
		Session: "s-1", Client: protocol.ClientClaudeCode, Kind: protocol.KindMod,
		Events: io.Discard, Log: io.Discard,
	})

	session.enqueue(reply{seq: 3, text: "третья"})
	session.enqueue(reply{seq: 2, text: "вторая"})
	session.enqueue(reply{seq: 1, text: "первая"})
	session.enqueue(reply{seq: 4, text: "четвёртая"})

	got := make([]string, 0, len(session.backlog))
	for _, one := range session.backlog {
		got = append(got, one.text)
	}

	want := []string{"первая", "вторая", "третья", "четвёртая"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("очередь не по порядку прихода: %v", got)
		}
	}
}
