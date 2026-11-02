package link_test

import (
	"net/http"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/link"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// chatName — название чата, которое средство сессии называет службе в тесте.
const chatName = "Проверка моста"

// TestNameReachesService: название чата доходит до службы готовым именем и той операцией, которая
// названа контрактом. Служба не знает, что это чат: имя ей передают обычное, а признак «задано
// вручную» она держит у себя.
func TestNameReachesService(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	waiting(t, srv)

	codes := make(chan int, 1)
	answers := make(chan link.Answer, 1)

	go func() {
		status, answer := mod.post(t, link.PathName, link.Name{Name: chatName})
		codes <- status

		answers <- answer
	}()

	frame := srv.take(t)
	if frame.Op != protocol.OpNameReport || frame.Name != chatName {
		t.Fatalf("имя дошло не таким: %+v", frame)
	}

	srv.ok(t, frame)

	status, answer := <-codes, <-answers
	if status != http.StatusOK || !answer.OK {
		t.Fatalf("местный вход ответил %d %+v", status, answer)
	}
}

// TestEmptyNameReachesService: пустое имя — не отказ, а сведение «у чата названия нет»: служба
// решает по нему, оставлять ли имя темы прежним.
func TestEmptyNameReachesService(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	waiting(t, srv)

	codes := make(chan int, 1)
	answers := make(chan link.Answer, 1)

	go func() {
		status, answer := mod.post(t, link.PathName, link.Name{Name: ""})
		codes <- status

		answers <- answer
	}()

	frame := srv.take(t)
	if frame.Op != protocol.OpNameReport || frame.Name != "" {
		t.Fatalf("пустое имя не дошло: %+v", frame)
	}

	srv.ok(t, frame)

	status, answer := <-codes, <-answers
	if status != http.StatusOK || !answer.OK {
		t.Fatalf("пустое имя отбито: %d %+v", status, answer)
	}
}
