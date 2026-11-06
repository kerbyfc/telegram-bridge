package link_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/link"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// topic — имя темы, которую служба называет в тесте.
const topic = "tg-inventory"

// first — id первой доставки в тесте.
const first = "d-7f31"

// session — id сессии в тесте: тот же id, который клиент кладёт в свой вход.
const session = "s-1"

// TestHelloNamesSessionAndMod: первый кадр соединения называет роль сессии, вид звонящего и
// клиента — роль сессии занимает только мод, потому что сессии без мода не бывает. Режим и тема
// приходят ответом, и средство сессии узнаёт их событием `ready` вместе с адресом местного входа.
func TestHelloNamesSessionAndMod(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)

	hello := srv.take(t)
	greeting(t, hello)

	resp := protocol.Ok(hello.RID)
	resp.Mode = protocol.ModeOn
	resp.TopicID = 42
	resp.TopicName = topic
	resp.Revived = true
	srv.reply(t, resp)

	ready := mod.event(t)
	opened(t, ready)

	if ready.Socket == "" || ready.Socket != mod.link.Front() {
		t.Fatalf("адрес местного входа не назван: %q при %q", ready.Socket, mod.link.Front())
	}
}

// TestHonourWaitsForAck: честь ставит подтверждённый приём, а не выдача. Пока средство сессии не
// подтвердило, в службу не уходит ничего — ни подтверждения, ни нового долгого запроса, — и
// слово владельца остаётся в ящике придержанным.
func TestHonourWaitsForAck(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	wait := srv.take(t)
	if wait.Op != protocol.OpDeliveryWait || wait.After != "" {
		t.Fatalf("долгий запрос доставки пришёл не таким: %+v", wait)
	}

	srv.hand(t, wait, first, "посмотри на диффе")
	handed(t, mod.event(t))

	// Выдача приёмом не считается: до подтверждения службе не уходит ни одного кадра.
	srv.silent(t)

	codes := make(chan int, 1)
	answers := make(chan link.Answer, 1)

	go func() {
		status, answer := mod.ack(t, link.Ack{
			DeliveryID: first, Outcome: protocol.AckAccepted, Detail: "",
		})
		codes <- status

		answers <- answer
	}()

	confirm := srv.take(t)
	if confirm.Op != protocol.OpDeliveryAck || confirm.DeliveryID != first ||
		confirm.Outcome != protocol.AckAccepted {
		t.Fatalf("подтверждение приёма пришло не таким: %+v", confirm)
	}

	srv.ok(t, confirm)

	status, answer := <-codes, <-answers
	if status != http.StatusOK || !answer.OK {
		t.Fatalf("местный вход ответил %d %+v", status, answer)
	}

	// Подтвердив, мод сразу же зовёт новый долгий запрос, и курсор помнит принятую доставку.
	next := srv.take(t)
	if next.Op != protocol.OpDeliveryWait || next.After != first {
		t.Fatalf("новый долгий запрос пришёл не таким: %+v", next)
	}
}

// TestRejectedDeliveryKeepsCursor: подача у агента не разрешилась — служба получает `rejected` с
// причиной, курсор не двигается, и слово владельца остаётся в ящике.
func TestRejectedDeliveryKeepsCursor(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	wait := srv.take(t)
	srv.command(t, wait, "d-9", "/compact")

	delivery := mod.event(t)
	if delivery.Kind != protocol.DeliveryClientCmd || delivery.Text != "/compact" {
		t.Fatalf("команда выдана не такой: %+v", delivery)
	}

	go mod.ack(t, link.Ack{
		DeliveryID: "d-9", Outcome: protocol.AckRejected, Detail: "команда не исполнилась",
	})

	confirm := srv.take(t)
	if confirm.Outcome != protocol.AckRejected || confirm.Detail != "команда не исполнилась" {
		t.Fatalf("отказ приёма пришёл не таким: %+v", confirm)
	}

	srv.ok(t, confirm)

	next := srv.take(t)
	if next.After != "" {
		t.Fatalf("курсор сдвинулся на непринятой доставке: %q", next.After)
	}
}

// TestAckOfOtherDeliveryRefused: подтверждение не на выданную доставку в службу не уходит —
// иначе честь встала бы на слове, которого агент не получал.
func TestAckOfOtherDeliveryRefused(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	wait := srv.take(t)
	srv.hand(t, wait, "d-1", "первое слово")
	mod.event(t)

	status, answer := mod.ack(t, link.Ack{
		DeliveryID: "d-2", Outcome: protocol.AckAccepted, Detail: "",
	})
	if status != http.StatusConflict || answer.OK {
		t.Fatalf("подтверждение чужой доставки прошло: %d %+v", status, answer)
	}

	srv.silent(t)
}

// TestBadAckRefused: негодное подтверждение — явный отказ местного входа, а не тихо принятое
// слово: исход у приёма один из двух.
func TestBadAckRefused(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	wait := srv.take(t)
	srv.hand(t, wait, "d-1", "первое слово")
	mod.event(t)

	status, answer := mod.ack(t, link.Ack{DeliveryID: "d-1", Outcome: "принял", Detail: ""})
	if status != http.StatusBadRequest || answer.OK {
		t.Fatalf("негодный исход приёма прошёл: %d %+v", status, answer)
	}

	srv.silent(t)
}

// TestServiceRefusalReachesMod: отказ службы на подтверждении доходит до средства сессии со
// своим кодом и кончает соединение. Отбивает подтверждение только та служба, для которой это
// соединение больше не сессии, а вытеснение сокет не закрывает — значит висеть на нём нельзя.
func TestServiceRefusalReachesMod(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	wait := srv.take(t)
	srv.hand(t, wait, "d-1", "первое слово")
	mod.event(t)

	answered := make(chan link.Answer, 1)
	codes := make(chan int, 1)

	go func() {
		status, answer := mod.ack(t, link.Ack{
			DeliveryID: "d-1", Outcome: protocol.AckAccepted, Detail: "",
		})
		codes <- status

		answered <- answer
	}()

	confirm := srv.take(t)
	srv.fail(t, confirm, protocol.CodeBadRequest, "`delivery.ack` зовёт не соединение сессии")

	if status := <-codes; status != http.StatusBadGateway {
		t.Fatalf("отказ службы ответил кодом %d", status)
	}

	answer := <-answered
	if answer.OK || answer.Code != protocol.CodeBadRequest {
		t.Fatalf("отказ службы дошёл не таким: %+v", answer)
	}

	fault := mod.event(t)
	if fault.Event != link.EventError || fault.Code != protocol.CodeBadRequest {
		t.Fatalf("об отказе на подтверждении сказано не так: %+v", fault)
	}

	err := mod.ended(t)
	if !errors.Is(err, link.ErrRefused) {
		t.Fatalf("после отказа на подтверждении соединение кончилось не отказом: %v", err)
	}

	if srv.opened() != 1 {
		t.Fatalf("после отказа на подтверждении соединение звонило снова: соединений %d", srv.opened())
	}
}

// TestVersionMismatchStops: расхождение мажора контракта — не повод звонить снова. Средство
// сессии узнаёт код отказа событием и говорит о нём владельцу своими средствами.
func TestVersionMismatchStops(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)

	hello := srv.take(t)
	srv.fail(t, hello, protocol.CodeVersion, "служба v2, звонящий v1")

	fault := mod.event(t)
	if fault.Event != link.EventError || fault.Code != protocol.CodeVersion {
		t.Fatalf("о расхождении версий сказано не так: %+v", fault)
	}

	err := mod.ended(t)
	if !errors.Is(err, link.ErrVersion) {
		t.Fatalf("соединение кончилось не расхождением версий: %v", err)
	}

	if srv.opened() != 1 {
		t.Fatalf("на чужой версии открыто соединений: %d", srv.opened())
	}
}

// TestDisplacedLinkStops: вытесненное соединение сессии узнаёт об этом концом своего долгого
// запроса и новым не звонит — иначе два мода отбирали бы сессию друг у друга по кругу.
func TestDisplacedLinkStops(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	wait := srv.take(t)
	srv.fail(t, wait, protocol.CodeBadRequest, "соединение больше не держит сессию")

	fault := mod.event(t)
	if fault.Event != link.EventError || fault.Code != protocol.CodeBadRequest {
		t.Fatalf("о вытеснении сказано не так: %+v", fault)
	}

	err := mod.ended(t)
	if !errors.Is(err, link.ErrRefused) {
		t.Fatalf("вытесненное соединение кончилось не отказом: %v", err)
	}

	if srv.opened() != 1 {
		t.Fatalf("вытесненное соединение звонило снова: соединений %d", srv.opened())
	}
}

// TestReconnectAfterDrop: перезапуск самой службы сессий не хоронит, и мод обязан вернуться —
// новое соединение называет ту же сессию.
func TestReconnectAfterDrop(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	srv.take(t)
	srv.drop(t)

	lost := mod.event(t)
	if lost.Event != link.EventLost {
		t.Fatalf("о потере соединения сказано не так: %+v", lost)
	}

	hello := srv.take(t)
	if hello.Op != protocol.OpHello || hello.Session != session {
		t.Fatalf("новое соединение назвалось не так: %+v", hello)
	}
}

// TestByeEndsLink: сессия кончилась — служба слышит `bye`, и подкоманда кончается вместе с ней.
func TestByeEndsLink(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	srv.take(t)

	go mod.bye(t)

	farewell := srv.take(t)
	if farewell.Op != protocol.OpBye {
		t.Fatalf("на конце сессии пришёл кадр `%s`", farewell.Op)
	}

	srv.ok(t, farewell)

	err := mod.ended(t)
	if err != nil {
		t.Fatalf("после `bye` соединение кончилось отказом: %v", err)
	}
}

// TestStopSaysByeForClaudeCode: на выходе Claude Code гасит дочерние процессы мода раньше, чем
// зовёт `session.end`, и `bye` мода уходил бы в мёртвый сокет. Поэтому остановка подкоманды у
// Claude Code — конец сессии: служба слышит `bye` по ещё открытому соединению, а подкоманда
// кончается без отказа.
func TestStopSaysByeForClaudeCode(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	srv.take(t)

	mod.stop()

	farewell := srv.take(t)
	if farewell.Op != protocol.OpBye {
		t.Fatalf("на остановке пришёл кадр `%s`", farewell.Op)
	}

	srv.ok(t, farewell)

	err := mod.ended(t)
	if err != nil {
		t.Fatalf("после остановки соединение кончилось отказом: %v", err)
	}
}

// TestStopFarewellHasDeadline: служба на `bye` остановки не отвечает — подкоманда всё равно
// кончается в срок `StopFarewell`, а не висит: иначе на ней висел бы выход клиента.
func TestStopFarewellHasDeadline(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)
	srv.take(t)

	began := time.Now()

	mod.stop()

	if farewell := srv.take(t); farewell.Op != protocol.OpBye {
		t.Fatalf("на остановке пришёл кадр `%s`", farewell.Op)
	}

	err := mod.ended(t)
	if err != nil {
		t.Fatalf("после остановки соединение кончилось отказом: %v", err)
	}

	if took := time.Since(began); took > stopFarewell+quiet {
		t.Fatalf("подкоманда кончилась через %v при сроке `bye` %v", took, stopFarewell)
	}
}

// TestStopWithoutByeForOpenCode: у OpenCode сервер живёт дальше сессии, и плагин гасит соединение
// без `bye` нарочно — служба хоронит такую сессию отсрочкой возврата. Остановка подкоманды
// адаптера кадра `bye` не шлёт.
func TestStopWithoutByeForOpenCode(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	adapter := startAs(t, srv, protocol.ClientOpenCode, protocol.KindAdapter)
	attach(t, srv, adapter)
	srv.take(t)

	adapter.stop()

	err := adapter.ended(t)
	if err != nil {
		t.Fatalf("после остановки соединение адаптера кончилось отказом: %v", err)
	}

	srv.silent(t)
}

// TestAdapterOfOpenCodeHoldsSessionLikeMod: плагин-адаптер OpenCode держит соединение сессии той
// же подкомандой и тем же протоколом, что мод. Разницы в кадрах нет ни одной, кроме имени клиента
// и вида звонящего в приветствии: отметки доставки, курсор и подтверждение приёма у обоих
// клиентов одни — по теме не видно, какой клиент за сессией.
func TestAdapterOfOpenCodeHoldsSessionLikeMod(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	adapter := startAs(t, srv, protocol.ClientOpenCode, protocol.KindAdapter)

	hello := srv.take(t)
	adapterGreeting(t, hello)

	resp := protocol.Ok(hello.RID)
	resp.Mode = protocol.ModeOn
	resp.TopicID = 42
	resp.TopicName = topic
	srv.reply(t, resp)

	ready := adapter.event(t)
	if ready.Event != link.EventReady || ready.TopicName != topic || ready.Socket == "" {
		t.Fatalf("соединение адаптера не открылось: %+v", ready)
	}

	wait := srv.take(t)
	if wait.Op != protocol.OpDeliveryWait {
		t.Fatalf("адаптер не встал на долгий запрос доставки: %+v", wait)
	}

	srv.hand(t, wait, first, "посмотри на диффе")
	handed(t, adapter.event(t))

	go adapter.ack(t, link.Ack{DeliveryID: first, Outcome: protocol.AckAccepted, Detail: ""})

	confirm := srv.take(t)
	if confirm.Op != protocol.OpDeliveryAck || confirm.Outcome != protocol.AckAccepted {
		t.Fatalf("подтверждение приёма адаптера пришло не таким: %+v", confirm)
	}

	srv.ok(t, confirm)

	next := srv.take(t)
	if next.Op != protocol.OpDeliveryWait || next.After != first {
		t.Fatalf("курсор адаптера после принятой доставки: %+v", next)
	}
}

// adapterGreeting сверяет приветствие плагина OpenCode: роль та же, что у мода, а отличается оно
// только именем клиента и видом звонящего.
func adapterGreeting(t *testing.T, hello protocol.Request) {
	t.Helper()

	if hello.Op != protocol.OpHello || hello.V != protocol.Version ||
		hello.Role != protocol.RoleSession || hello.Session != session {
		t.Fatalf("приветствие адаптера назвалось не так: %+v", hello)
	}

	if hello.Kind != string(protocol.KindAdapter) || hello.Client != string(protocol.ClientOpenCode) {
		t.Fatalf("адаптер назвался видом %q клиента %q", hello.Kind, hello.Client)
	}
}

// greeting сверяет первый кадр соединения: версия, роль, вид звонящего, сессия и клиент.
func greeting(t *testing.T, hello protocol.Request) {
	t.Helper()

	if hello.Op != protocol.OpHello {
		t.Fatalf("первый кадр — `%s`, а не `hello`", hello.Op)
	}

	if hello.V != protocol.Version || hello.Role != protocol.RoleSession ||
		hello.Kind != string(protocol.KindMod) || hello.Session != session ||
		hello.Client != string(protocol.ClientClaudeCode) {
		t.Fatalf("приветствие назвалось не так: %+v", hello)
	}
}

// opened сверяет событие готовности: режим и тема приходят ответом на приветствие.
func opened(t *testing.T, ready link.Event) {
	t.Helper()

	if ready.Event != link.EventReady || ready.Mode != protocol.ModeOn ||
		ready.TopicName != topic || !ready.Revived {
		t.Fatalf("событие готовности назвало не то: %+v", ready)
	}
}

// handed сверяет выданную доставку: провенанс мод приклеивает к подаче как есть.
func handed(t *testing.T, delivery link.Event) {
	t.Helper()

	if delivery.Event != link.EventDelivery || delivery.DeliveryID != first ||
		delivery.Text != "посмотри на диффе" || delivery.Kind != protocol.DeliveryOwnerMessage {
		t.Fatalf("доставка выдана не такой: %+v", delivery)
	}

	if delivery.Provenance == "" || delivery.Author == nil || delivery.Topic == nil {
		t.Fatalf("доставка выдана без провенанса: %+v", delivery)
	}
}

// attach проводит приветствие и дожидается события готовности.
func attach(t *testing.T, srv *service, mod *probe) {
	t.Helper()

	hello := srv.take(t)

	resp := protocol.Ok(hello.RID)
	resp.Mode = protocol.ModeOn
	resp.TopicID = 42
	resp.TopicName = topic
	srv.reply(t, resp)

	ready := mod.event(t)
	if ready.Event != link.EventReady {
		t.Fatalf("соединение не открылось: %+v", ready)
	}
}

// TestServiceRestartKeepsLink: остановка службы рвёт соединение без ответа, сокета какое-то время
// нет вовсе, а подкоманда в том же процессе звонит снова и возвращает сессию приветствием. Потеря
// сказана одним событием на всю пропажу, а не на каждую попытку, и отказа нет.
func TestServiceRestartKeepsLink(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	wait := srv.take(t)
	if wait.Op != protocol.OpDeliveryWait {
		t.Fatalf("долгий запрос не пришёл: %+v", wait)
	}

	srv.vanish(t)

	lost := mod.event(t)
	if lost.Event != link.EventLost {
		t.Fatalf("о потере соединения сказано не так: %+v", lost)
	}

	// Службы нет десятки пауз: попытки идут, а событий за них нет.
	mod.still(t, 20*retry)

	srv.revive(t)

	hello := srv.take(t)
	if hello.Op != protocol.OpHello || hello.Session != session {
		t.Fatalf("после перезапуска сессия названа не так: %+v", hello)
	}

	resp := protocol.Ok(hello.RID)
	resp.Mode = protocol.ModeOn
	srv.reply(t, resp)

	ready := mod.event(t)
	if ready.Event != link.EventReady || ready.Mode != protocol.ModeOn {
		t.Fatalf("сессия после перезапуска не вернулась с режимом: %+v", ready)
	}

	if again := srv.take(t); again.Op != protocol.OpDeliveryWait {
		t.Fatalf("после возврата нет долгого запроса: %+v", again)
	}

	mod.running(t)
}

// TestLongAbsenceSaidOnce: службы нет дольше срока молчания — средство сессии узнаёт об этом одним
// событием `absent`, подкоманда звонит дальше, и вернувшаяся служба получает ту же сессию.
func TestLongAbsenceSaidOnce(t *testing.T) {
	t.Parallel()

	const absent = 5 * retry

	srv := listen(t)
	mod := startWith(t, srv, protocol.ClientClaudeCode, protocol.KindMod, absent)
	attach(t, srv, mod)

	srv.take(t)
	srv.vanish(t)

	if lost := mod.event(t); lost.Event != link.EventLost {
		t.Fatalf("о потере соединения сказано не так: %+v", lost)
	}

	gone := mod.event(t)
	if gone.Event != link.EventAbsent || gone.Text == "" {
		t.Fatalf("о долгой пропаже службы сказано не так: %+v", gone)
	}

	mod.still(t, 4*absent)

	srv.revive(t)
	attach(t, srv, mod)
	mod.running(t)
}

// TestRestartWithReplyIsRefusal: служба, которая на обрыве всё же ответила отказом долгому запросу,
// — это отказ, а не потеря: различать их подкоманде нечем, и молча звонить снова на отказ нельзя.
// Поэтому остановка службы обязана рвать соединение без ответа (транспорт службы).
func TestRestartWithReplyIsRefusal(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	wait := srv.take(t)
	srv.fail(t, wait, protocol.CodeBadRequest, "соединение закрылось")

	if fault := mod.event(t); fault.Event != link.EventError {
		t.Fatalf("отказ долгому запросу сказан не так: %+v", fault)
	}

	err := mod.ended(t)
	if !errors.Is(err, link.ErrRefused) {
		t.Fatalf("отказ долгому запросу кончил подкоманду не отказом: %v", err)
	}
}

// TestByeOutrunsBurial: похороны на службе кончают долгий запрос отказом раньше, чем приходит
// ответ на `bye`. Этот отказ — конец сессии, а не вытеснение: средство сессии получает ответ на свой
// `bye`, события `error` нет, и подкоманда кончается без отказа.
func TestByeOutrunsBurial(t *testing.T) {
	t.Parallel()

	srv := listen(t)
	mod := start(t, srv)
	attach(t, srv, mod)

	wait := srv.take(t)

	answered := make(chan link.Answer, 1)

	go func() {
		_, answer := mod.bye(t)
		answered <- answer
	}()

	farewell := srv.take(t)
	if farewell.Op != protocol.OpBye {
		t.Fatalf("вместо `bye` пришёл кадр `%s`", farewell.Op)
	}

	srv.fail(t, wait, protocol.CodeBadRequest, "соединение больше не держит сессию")

	// Ответ на `bye` служба отдаёт позже: похороны закрывают тему, и это вызов платформы.
	time.Sleep(quiet)
	srv.ok(t, farewell)

	select {
	case answer := <-answered:
		if !answer.OK {
			t.Fatalf("`bye` не получил ответа службы: %+v", answer)
		}
	case <-time.After(patience):
		t.Fatal("ответа на `bye` не дождались")
	}

	err := mod.ended(t)
	if err != nil {
		t.Fatalf("после `bye` подкоманда кончилась отказом: %v", err)
	}

	mod.still(t, quiet)
}
