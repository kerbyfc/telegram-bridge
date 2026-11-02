package core_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestLivenessIsConnectionNotPID: живость держит открытое соединение сессии, и ничего кроме.
// Номера процесса в приветствии нет вовсе — решать живость по нему нечем.
func TestLivenessIsConnectionNotPID(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	before := len(rig.api.texts(on.TopicID))

	time.Sleep(2 * rig.cfg.ReconnectGrace)
	rig.reap()

	status := rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.Mode != protocol.ModeOn {
		t.Fatalf("живая сессия похоронена при живом соединении: %+v", status)
	}

	rig.closeConn(conn)
	rig.reap()

	status = rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.Mode != protocol.ModeOn {
		t.Fatal("сессия похоронена до истечения отсрочки возврата")
	}

	time.Sleep(2 * rig.cfg.ReconnectGrace)
	rig.reap()

	// Сессия похоронена — это видно её возврату, — но режим похороны не снимают: он свойство
	// темы и её сессии, и снимает его только слово владельца.
	_, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.Revived {
		t.Fatalf("сессия не похоронена после отсрочки: %+v", back)
	}

	if back.Mode != protocol.ModeOn {
		t.Fatalf("похороны сняли режим: %+v", back)
	}

	// Служебных строк о жизни сессии в теме нет вовсе: состояние владелец видит значком темы и
	// вкладом в статусную строку, а в теме остались только слова агента.
	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("служба написала в тему об уходе сессии: %v", texts[before:])
	}

	if !rig.api.inModeOf(on.TopicID) {
		t.Fatal("значок темы снят похоронами, хотя режим остался")
	}
}

// TestRestartGivesFullGrace: перезапуск службы живых сессий не хоронит — после её старта у
// каждой есть полная отсрочка на возврат.
func TestRestartGivesFullGrace(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, _ := rig.mod("s1", topicAlpha)
	rig.closeConn(conn)

	// Обрыв был давно: без правила «от старта службы» эта сессия была бы похоронена сразу.
	time.Sleep(2 * rig.cfg.ReconnectGrace)
	rig.reopen()
	rig.reap()

	fresh, hello := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !hello.OK {
		t.Fatalf("сессия не вернулась: %+v", hello.Error)
	}

	if hello.Revived {
		t.Fatal("перезапуск службы похоронил живую сессию")
	}

	if hello.Mode != protocol.ModeOn {
		t.Fatalf("режим вернувшейся сессии снят: %+v", hello)
	}

	status := rig.do(fresh, rig.req(protocol.OpModeStatus))
	if status.TopicName != topicAlpha {
		t.Fatalf("привязка к теме потеряна: %+v", status)
	}
}

// TestRestartBuriesOnlyAfterGrace: не вернулась за отсрочку — режим снимает сама служба.
func TestRestartBuriesOnlyAfterGrace(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, _ := rig.mod("s1", topicAlpha)
	rig.closeConn(conn)
	rig.reopen()

	time.Sleep(2 * rig.cfg.ReconnectGrace)
	rig.reap()

	_, hello := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !hello.Revived {
		t.Fatalf("сессия не похоронена после отсрочки: %+v", hello)
	}
}

// TestRevivedSessionKeepsMode: похороненная сессия возвращается с режимом — смерть его не
// снимает. В тему о возврате служба не пишет и в сессию ничего не подаёт: о самом возврате
// средство сессии узнаёт полем `revived` в ответе на своё же `hello`.
func TestRevivedSessionKeepsMode(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))

	before := len(rig.api.texts(on.TopicID))

	_, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)

	if !back.Revived || back.Mode != protocol.ModeOn {
		t.Fatalf("возврат похороненной сессии не тот: %+v", back)
	}

	if back.TopicID != on.TopicID {
		t.Fatalf("привязка к теме не цела: %+v", back)
	}

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("служба написала в тему о возврате сессии: %v", texts[before:])
	}
}

// TestDisplacedByeKeepsSession: перезагрузка мода — гонка «новый `hello` раньше `bye` прежнего».
// Погашенная подкоманда говорит `bye` по уже вытесненному соединению: служба его отбивает, сессия
// жива, а долгий запрос нового соединения не отпущен и получает слово владельца.
func TestDisplacedByeKeepsSession(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	first, on := rig.mod("s1", topicAlpha)

	second, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.OK || back.Revived {
		t.Fatalf("вернувшийся мод встал не так: %+v", back)
	}

	waiting := make(chan protocol.Response, 1)

	go func() { waiting <- rig.waitDelivery(second) }()

	time.Sleep(20 * time.Millisecond)

	if resp := rig.do(first, rig.req(protocol.OpBye)); resp.OK {
		t.Fatal("вытесненное соединение похоронило сессию своим `bye`")
	}

	select {
	case resp := <-waiting:
		t.Fatalf("`bye` вытесненного соединения отпустил долгий запрос нового: %+v", resp)
	case <-time.After(50 * time.Millisecond):
	}

	rig.ownerMessage(on.TopicID, "слово после перезагрузки")

	got := <-waiting
	if got.Text != "слово после перезагрузки" {
		t.Fatalf("новое соединение доставки не получило: %+v", got)
	}

	for _, entry := range rig.journalEvents() {
		if entry.Event == "session-buried" {
			t.Fatalf("сессия похоронена: %+v", entry)
		}
	}
}

// TestByeThenHelloKeepsEverything: перезагрузка мода без гонки — `bye` прежней подкоманды хоронит
// сессию, `hello` новой поднимает её с `revived`. Ни режим, ни тема, ни ящик при этом не теряются:
// слово, выданное прежней подкоманде без подтверждения, и слово, пришедшее между похоронами и
// возвратом, уезжают новому соединению.
func TestByeThenHelloKeepsEverything(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	first, on := rig.mod("s1", topicAlpha)

	rig.ownerMessage(on.TopicID, "первое")

	if handed := rig.waitDelivery(first); handed.Text != "первое" {
		t.Fatalf("прежней подкоманде слово не выдано: %+v", handed)
	}

	if resp := rig.do(first, rig.req(protocol.OpBye)); !resp.OK {
		t.Fatalf("`bye` держателя отбит: %+v", resp.Error)
	}

	rig.ownerMessage(on.TopicID, "второе")

	second, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.Revived || back.Mode != protocol.ModeOn || back.TopicID != on.TopicID || back.TopicName != topicAlpha {
		t.Fatalf("возврат после `bye` не тот: %+v", back)
	}

	if files := rig.inboxFiles("s1"); len(files) != 2 {
		t.Fatalf("ящик после похорон не цел: %v", files)
	}

	for _, want := range []string{"первое", "второе"} {
		got := rig.waitDelivery(second)
		if got.Text != want {
			t.Fatalf("новому соединению уехало %+v, ждали %q", got, want)
		}

		rig.ack(second, got.DeliveryID, protocol.AckAccepted, "")
	}
}

// TestModeSurvivesLostConn: обрыв соединения режима не снимает — подключившаяся сессия застаёт
// его прежним, и придержанные слова уезжают моду первым же долгим запросом.
func TestModeSurvivesLostConn(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	first, on := rig.mod("s1", topicAlpha)

	rig.ownerMessage(on.TopicID, "посмотри")
	rig.closeConn(first)

	time.Sleep(2 * rig.cfg.ReconnectGrace)
	rig.reap()

	second, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if back.Mode != protocol.ModeOn {
		t.Fatalf("режим не дожил до возврата сессии: %+v", back)
	}

	got := rig.waitDelivery(second)
	if got.Text != "посмотри" {
		t.Fatalf("придержанное слово не уехало вернувшейся сессии: %+v", got)
	}
}

// TestTopicCommandNeedsNoLiveSession: команда темы живого соединения не требует. Из темы режим
// включается всегда — состояние ложится на запись темы, а сессия застаёт его при подключении.
func TestTopicCommandNeedsNoLiveSession(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	// Режим снят владельцем, сессия ушла и похоронена: живого соединения у темы нет.
	rig.ownerMessage(on.TopicID, "/here")
	rig.closeConn(conn)
	time.Sleep(2 * rig.cfg.ReconnectGrace)
	rig.reap()

	before := len(rig.api.texts(on.TopicID))
	rig.ownerMessage(on.TopicID, "/away")

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("команда темы отбита у неживой сессии: %v", texts[before:])
	}

	if !rig.api.inModeOf(on.TopicID) {
		t.Fatal("значок темы не в режиме после `/away` из темы")
	}

	back, hello := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if hello.Mode != protocol.ModeOn {
		t.Fatalf("сессия не застала включённый из темы режим: %+v", hello)
	}

	rig.ownerMessage(on.TopicID, "слово после возврата")

	got := rig.waitDelivery(back)
	if got.Text != "слово после возврата" {
		t.Fatalf("слово владельца не уехало моду: %+v", got)
	}
}

// TestShortCallSwitchesModeButHoldsNoLiveness: тонкий клиент режимом управляет, а живости не
// несёт. Он живёт секунду и закрывается — режим от этого включён и остаётся включённым, но
// живость держит только соединение мода, и его уход хоронит сессию.
func TestShortCallSwitchesModeButHoldsNoLiveness(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, _ := rig.mod("s1", topicAlpha)

	tool := rig.tool("s1")
	off := rig.req(protocol.OpModeOff)
	off.Session = "s1"
	rig.do(tool, off)

	on := rig.req(protocol.OpModeOn)
	on.Session = "s1"
	rig.do(tool, on)

	status := rig.req(protocol.OpModeStatus)
	status.Session = "s1"

	if got := rig.do(tool, status); got.Mode != protocol.ModeOn {
		t.Fatalf("тонкий клиент режимом не управляет: %+v", got)
	}

	// Тонкий клиент закрылся, мод ушёл — живость кончилась вместе с его соединением, и служба
	// сессию похоронила. Режим при этом остался: его снимает только слово владельца.
	rig.closeConn(tool)
	rig.closeConn(mod)
	time.Sleep(2 * rig.cfg.ReconnectGrace)
	rig.reap()

	buried := false

	for _, entry := range rig.journalEvents() {
		if entry.Event == "session-buried" && entry.Session == "s1" {
			buried = true
		}
	}

	if !buried {
		t.Fatal("сессия осталась живой без соединения мода")
	}

	if got := rig.do(tool, status); got.Mode != protocol.ModeOn {
		t.Fatalf("похороны сняли режим: %+v", got)
	}
}

// TestModeOnForUnknownSessionIsRefused: сессия, которой служба не знает, — сессия без мода, то
// есть поломка установки. Честный ответ на такой вызов — отказ с названной причиной.
func TestModeOnForUnknownSessionIsRefused(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	tool := rig.tool("s-нет")

	req := rig.req(protocol.OpModeOn)
	req.Session = "s-нет"
	req.Name = topicAlpha

	resp := rig.do(tool, req)
	if resp.OK || resp.Error.Code != protocol.CodeBadRequest {
		t.Fatalf("режим включён сессии без мода: %+v", resp)
	}
}

// TestStateKeepsNoProcessMarkers: в состоянии сессии нет ни номера процесса, ни полей прежней
// живости — их не читает ни одно правило контракта, а номер процесса прямо противоречит живости.
func TestStateKeepsNoProcessMarkers(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.mod("s1", topicAlpha)

	body, err := os.ReadFile(filepath.Join(rig.root, "sessions", "s1.json"))
	if err != nil {
		t.Fatalf("привязка сессии не прочитана: %v", err)
	}

	for _, needle := range []string{"pid", "last_call", "had_conn"} {
		if strings.Contains(string(body), needle) {
			t.Fatalf("в состоянии сессии осталось %q: %s", needle, body)
		}
	}
}

// TestCoreNeverChecksProcesses: сторож текста, а не поведения. Он читает только файлы
// `internal/core`, ищет в них шесть подстрок и в соседние пакеты не смотрит — доказательством
// он не служит, живость доказывают поведенческие тесты выше. Нужен он затем, чтобы решение по
// номеру процесса не вернулось в ядро незаметно для ревью.
func TestCoreNeverChecksProcesses(t *testing.T) {
	t.Parallel()

	forbidden := []string{"os.FindProcess", "syscall.Kill", "Signal(", "req.PID", "/proc/"}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("каталог ядра не прочитан: %v", err)
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		body, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatalf("файл ядра не прочитан: %v", err)
		}

		for _, needle := range forbidden {
			if strings.Contains(string(body), needle) {
				t.Fatalf("%s: ядро смотрит на процесс через %q", entry.Name(), needle)
			}
		}
	}
}
