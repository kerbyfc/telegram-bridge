package link_test

import (
	"net/http"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/link"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// shownPercent — процент контекста, который средство сессии называет в тесте.
const shownPercent = 42

// TestTurnBordersReachService: границы хода доходят до службы теми операциями протокола, которые
// названы контрактом, и несут имя хода — по нему служба гасит «печатает» только своего хода.
// Своего кадра у границы нет: операцию называет сам путь местного входа, и путь этот один у мода
// Claude Code и у плагина OpenCode.
func TestTurnBordersReachService(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	waiting(t, srv)

	border(t, srv, mod, link.PathTurnStart, protocol.OpTurnStart)
	border(t, srv, mod, link.PathTurnEnd, protocol.OpTurnEnd)
}

// TestContextReportCarriesPercent: процент контекста доходит до службы готовым числом. Своего
// счёта контекста у подкоманды нет вовсе: она проводит то, что назвал клиент.
func TestContextReportCarriesPercent(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	waiting(t, srv)

	codes := make(chan int, 1)
	answers := make(chan link.Answer, 1)

	go func() {
		status, answer := mod.post(t, link.PathContext, link.Report{Percent: shownPercent})
		codes <- status

		answers <- answer
	}()

	frame := srv.take(t)
	if frame.Op != protocol.OpContextReport || frame.Percent != shownPercent {
		t.Fatalf("процент дошёл не таким: %+v", frame)
	}

	srv.ok(t, frame)

	status, answer := <-codes, <-answers
	if status != http.StatusOK || !answer.OK {
		t.Fatalf("местный вход ответил %d %+v", status, answer)
	}
}

// TestBadContextReportRefused: процент вне сотни — отказ с названной причиной, а не правка строки
// состояния чем попало: до службы такой кадр не доходит вовсе.
func TestBadContextReportRefused(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	waiting(t, srv)

	status, answer := mod.post(t, link.PathContext, link.Report{Percent: 101})
	if status != http.StatusBadRequest || answer.OK {
		t.Fatalf("негодный процент принят: %d %+v", status, answer)
	}

	srv.silent(t)
}

// TestAgentMessageReachesService: дописанная реплика доходит до службы операцией потока и целиком
// — подкоманда её не режет, не склеивает и не правит ничего в теме.
func TestAgentMessageReachesService(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	waiting(t, srv)

	codes := make(chan int, 1)
	answers := make(chan link.Answer, 1)

	go func() {
		status, answer := mod.post(t, link.PathMessage, link.Message{Text: "читаю диффы"})
		codes <- status

		answers <- answer
	}()

	frame := srv.take(t)
	if frame.Op != protocol.OpAgentMessage || frame.Text != "читаю диффы" {
		t.Fatalf("реплика дошла не такой: %+v", frame)
	}

	srv.ok(t, frame)

	status, answer := <-codes, <-answers
	if status != http.StatusOK || !answer.OK {
		t.Fatalf("местный вход ответил %d %+v", status, answer)
	}
}

// TestEmptyAgentMessageRefused: реплика без текста — отказ с названной причиной, а не пустое
// сообщение в теме: до службы такой кадр не доходит вовсе.
func TestEmptyAgentMessageRefused(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	waiting(t, srv)

	status, answer := mod.post(t, link.PathMessage, link.Message{Text: ""})
	if status != http.StatusBadRequest || answer.OK {
		t.Fatalf("пустая реплика принята: %d %+v", status, answer)
	}

	srv.silent(t)
}

// waiting снимает долгий запрос доставки, который мод зовёт сразу за приветствием: дальше тест
// смотрит на кадры коротких запросов.
func waiting(t *testing.T, srv *service) {
	t.Helper()

	wait := srv.take(t)
	if wait.Op != protocol.OpDeliveryWait {
		t.Fatalf("мод не встал на долгий запрос доставки: %+v", wait)
	}
}

// border шлёт границу хода местным входом и сверяет, какой операцией и с каким именем хода она
// дошла до службы.
func border(t *testing.T, srv *service, mod *probe, path string, op protocol.Op) {
	t.Helper()

	codes := make(chan int, 1)
	answers := make(chan link.Answer, 1)

	go func() {
		status, answer := mod.post(t, path, link.Turn{ID: "t-1"})
		codes <- status

		answers <- answer
	}()

	frame := srv.take(t)
	if frame.Op != op {
		t.Fatalf("граница хода дошла операцией `%s`, а не `%s`", frame.Op, op)
	}

	if frame.Turn != "t-1" {
		t.Fatalf("граница хода дошла без имени хода: %+v", frame)
	}

	srv.ok(t, frame)

	status, answer := <-codes, <-answers
	if status != http.StatusOK || !answer.OK {
		t.Fatalf("местный вход ответил %d %+v", status, answer)
	}
}
