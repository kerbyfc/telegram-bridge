package core_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// sessionRecord читает запись сессии с диска.
func (r *rig) sessionRecord(id string) *store.Session {
	r.t.Helper()

	sessions, err := store.LoadSessions(r.store)
	if err != nil {
		r.t.Fatalf("привязки сессий не прочитаны: %v", err)
	}

	for _, sess := range sessions {
		if sess.ID == id {
			return sess
		}
	}

	r.t.Fatalf("записи сессии %s нет", id)

	return nil
}

// legacyBury делает из сессии погребённую прежней службой: запись на диске `buried` при режиме, а
// тема открыта и держит прежнее имя.
func (r *rig) legacyBury(id string) {
	r.t.Helper()

	sess := r.sessionRecord(id)
	sess.Buried = true

	err := sess.Save(r.store)
	if err != nil {
		r.t.Fatalf("запись сессии не сохранена: %v", err)
	}

	r.reopen()
}

// journalled отвечает, есть ли в журнале событие с такой причиной.
func (r *rig) journalled(event string, code protocol.Code) bool {
	r.t.Helper()

	for _, entry := range r.journalEvents() {
		if entry.Event == event && entry.Code == string(code) {
			return true
		}
	}

	return false
}

// TestByeSetsOffIconAndClosesTopic: сессия кончилась — значок выкл ставится до закрытия, имя темы не
// правится.
func TestByeSetsOffIconAndClosesTopic(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	before := len(rig.api.texts(on.TopicID))
	calls := len(rig.api.platformCalls())

	if resp := rig.do(conn, rig.req(protocol.OpBye)); !resp.OK {
		t.Fatalf("`bye` отбит: %+v", resp.Error)
	}

	if got := rig.api.platformCalls()[calls:]; !slices.Equal(got, []string{callIconOff, callClose}) {
		t.Fatalf("погребение звало платформу так: %v", got)
	}

	if !rig.api.closedOf(on.TopicID) {
		t.Fatal("тема кончившейся сессии не закрыта")
	}

	if got := rig.api.nameOf(on.TopicID); got != topicAlpha {
		t.Fatalf("имя темы кончившейся сессии: %q", got)
	}

	if got := rig.api.iconOf(on.TopicID); got != iconOff {
		t.Fatalf("значок темы кончившейся сессии: %q", got)
	}

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("служба написала в тему о конце сессии: %v", texts[before:])
	}

	// Погребение идемпотентно: повторный `bye` платформу больше не зовёт.
	calls = len(rig.api.platformCalls())
	rig.do(conn, rig.req(protocol.OpBye))

	if got := rig.api.platformCalls()[calls:]; len(got) != 0 {
		t.Fatalf("повторный `bye` тронул тему: %v", got)
	}
}

// TestExpiredGraceClosesTopic: сессия, не вернувшаяся за отсрочку, погребается тем же путём.
func TestExpiredGraceClosesTopic(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	rig.closeConn(conn)
	rig.reap()

	if rig.api.closedOf(on.TopicID) {
		t.Fatal("тема закрыта до истечения отсрочки возврата")
	}

	time.Sleep(2 * rig.cfg.ReconnectGrace)
	rig.reap()

	if !rig.api.closedOf(on.TopicID) || rig.api.iconOf(on.TopicID) != iconOff {
		t.Fatalf("тема не погребена: закрыта %v, значок %q", rig.api.closedOf(on.TopicID), rig.api.iconOf(on.TopicID))
	}
}

// TestDisplacedByeLeavesTopicOpen: отбитый `bye` вытесненного соединения никого не погребает.
func TestDisplacedByeLeavesTopicOpen(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	first, on := rig.mod("s1", topicAlpha)
	rig.hello("s1", protocol.RoleSession, protocol.KindMod)

	rig.do(first, rig.req(protocol.OpBye))

	if rig.api.closedOf(on.TopicID) || rig.api.nameOf(on.TopicID) != topicAlpha {
		t.Fatal("отбитый `bye` тронул тему живой сессии")
	}
}

// TestBurialLeavesNameAlone: погребение имени не правит — ни длинного, ни с номером.
func TestBurialLeavesNameAlone(t *testing.T) {
	t.Parallel()

	for _, name := range []string{topicAlpha, strings.Repeat("я", 300)} {
		rig := newRig(t)

		rig.mod("s1", name)
		conn, on := rig.mod("s2", name)
		held := rig.api.nameOf(on.TopicID)

		rig.do(conn, rig.req(protocol.OpBye))

		if got := rig.api.nameOf(on.TopicID); got != held || !strings.HasSuffix(got, "#2") {
			t.Fatalf("имя темы %q стало %q", held, got)
		}

		if got := rig.api.renamedNames(); len(got) != 0 {
			t.Fatalf("погребение переименовало тему: %v", got)
		}
	}
}

// TestBurialWithoutModeCallsOnlyClose: значок выкл уже стоит, и лишнего вызова значка нет.
func TestBurialWithoutModeCallsOnlyClose(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, _ := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpModeOff))

	calls := len(rig.api.platformCalls())

	rig.do(conn, rig.req(protocol.OpBye))

	if got := rig.api.platformCalls()[calls:]; !slices.Equal(got, []string{callClose}) {
		t.Fatalf("погребение без режима звало платформу так: %v", got)
	}
}

// TestReloadPlatformCalls: перезагрузка мода — `bye` и новое `hello`. Каждый вызов здесь — одна
// служебная строка в теме: в режиме их четыре, без режима две, и имя не правится вовсе.
func TestReloadPlatformCalls(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		inMode bool
		want   []string
	}{
		{"в режиме", true, []string{callIconOff, callClose, callReopen, callIconOn}},
		{"без режима", false, []string{callClose, callReopen}},
	}

	for _, one := range cases {
		rig := newRig(t)
		conn, on := rig.mod("s1", topicAlpha)

		if !one.inMode {
			rig.do(conn, rig.req(protocol.OpModeOff))
		}

		calls := len(rig.api.platformCalls())

		rig.do(conn, rig.req(protocol.OpBye))
		rig.hello("s1", protocol.RoleSession, protocol.KindMod)

		if got := rig.api.platformCalls()[calls:]; !slices.Equal(got, one.want) {
			t.Fatalf("%s: перезагрузка звала платформу так: %v", one.name, got)
		}

		if rig.api.nameOf(on.TopicID) != topicAlpha {
			t.Fatalf("%s: имя темы %q", one.name, rig.api.nameOf(on.TopicID))
		}
	}
}

// TestBuriedNameIsFreed: новая сессия с тем же названием получает его без суффикса, и своей новой
// темой: закрытая осталась кончившейся сессии.
func TestBuriedNameIsFreed(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, first := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))

	_, second := rig.mod("s2", topicAlpha)

	if second.TopicName != topicAlpha || !second.Created || second.TopicID == first.TopicID {
		t.Fatalf("новая сессия получила не то: %+v", second)
	}

	if !rig.api.closedOf(first.TopicID) || rig.api.nameOf(first.TopicID) != topicAlpha {
		t.Fatalf("закрытую тему тронули: %q", rig.api.nameOf(first.TopicID))
	}
}

// TestBoundSessionTakesFreedName: привязанная сессия, названная именем кончившейся, тоже получает
// его без суффикса: именно переименованием и рождалось `Имя#2`.
func TestBoundSessionTakesFreedName(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, _ := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))

	second, _ := rig.mod("s2", "")

	req := rig.req(protocol.OpNameReport)
	req.Name = topicAlpha
	rig.do(second, req)

	if got := rig.do(second, rig.req(protocol.OpModeStatus)); got.TopicName != topicAlpha {
		t.Fatalf("имя кончившейся сессии не освободилось: %+v", got)
	}
}

// TestBuriedNamesDoNotCollide: две кончившиеся сессии одного имени держат одно и то же видимое имя,
// а новая сессия получает его без суффикса.
func TestBuriedNamesDoNotCollide(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	for _, id := range []string{"s1", "s2", "s3"} {
		conn, on := rig.mod(id, topicAlpha)
		if on.TopicName != topicAlpha || !on.Created {
			t.Fatalf("%s получила %+v", id, on)
		}

		rig.do(conn, rig.req(protocol.OpBye))

		if got := rig.api.nameOf(on.TopicID); got != topicAlpha || !rig.api.closedOf(on.TopicID) {
			t.Fatalf("%s: имя темы %q, закрыта %v", id, got, rig.api.closedOf(on.TopicID))
		}
	}
}

// TestBuriedHoldsNoModeUntilReturn: кончившаяся сессия режим не держит и на диске, и в
// самопроверке, и в статусной строке, а возврат получает его таким, каким он был. Перезапуск службы
// между ними этого не теряет.
func TestBuriedHoldsNoModeUntilReturn(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))

	if rig.sessionRecord("s1").Mode {
		t.Fatal("запись кончившейся сессии держит режим")
	}

	tool := rig.tool("s1")

	status := rig.do(tool, rig.req(protocol.OpServiceStatus))
	if got := status.Service.Sessions[0]; got.Mode != protocol.ModeOff || got.Life != protocol.LifeDead {
		t.Fatalf("самопроверка видит сессию так: %+v", got)
	}

	rig.reopen()

	_, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.Revived || back.Mode != protocol.ModeOn || back.TopicID != on.TopicID || back.TopicName != topicAlpha {
		t.Fatalf("возврат не тот: %+v", back)
	}

	if rig.api.closedOf(on.TopicID) || rig.api.nameOf(on.TopicID) != topicAlpha {
		t.Fatalf("тема не вернулась: закрыта %v, имя %q", rig.api.closedOf(on.TopicID), rig.api.nameOf(on.TopicID))
	}

	if got := rig.api.iconOf(on.TopicID); got != iconOn {
		t.Fatalf("значок вернувшейся в режим сессии: %q", got)
	}

	if !rig.sessionRecord("s1").Mode {
		t.Fatal("запись вернувшейся сессии режима не держит")
	}
}

// TestBurialClearsContributionUntilReturn: режим кончившейся сессии не стоит в статусной строке, а
// вернувшаяся снова его там показывает.
func TestBurialClearsContributionUntilReturn(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, _ := rig.mod(modeSession, topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))

	if got := rig.contribution(); len(got.Items) != 0 {
		t.Fatalf("кончившаяся сессия осталась в статусной строке: %+v", got.Items)
	}

	rig.hello(modeSession, protocol.RoleSession, protocol.KindMod)

	if got := rig.contribution(); len(got.Items) != 1 || got.Items[0].Parts[0].Text != topicAlpha {
		t.Fatalf("вернувшаяся сессия не в статусной строке: %+v", got.Items)
	}
}

// TestReturnWithoutModeStaysWithout: сессия без режима и вернётся без него, а тему получит назад.
func TestReturnWithoutModeStaysWithout(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpModeOff))
	rig.do(conn, rig.req(protocol.OpBye))

	if !rig.api.closedOf(on.TopicID) {
		t.Fatal("тема сессии без режима не закрыта")
	}

	_, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if back.Mode != protocol.ModeOff || back.TopicName != topicAlpha || rig.api.closedOf(on.TopicID) {
		t.Fatalf("возврат не тот: %+v", back)
	}

	if got := rig.api.iconOf(on.TopicID); got != iconOff {
		t.Fatalf("значок вернувшейся без режима сессии: %q", got)
	}
}

// TestReturnToTakenNameGetsSuffix: имя заняла новая сессия — вернувшаяся берёт обычный суффикс.
func TestReturnToTakenNameGetsSuffix(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, first := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))

	_, second := rig.mod("s2", topicAlpha)

	_, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if back.TopicID != first.TopicID || back.TopicName != "alpha#2" {
		t.Fatalf("возврат не тот: %+v", back)
	}

	if rig.api.closedOf(first.TopicID) || rig.api.nameOf(first.TopicID) != "alpha#2" {
		t.Fatalf("тема на платформе: %q", rig.api.nameOf(first.TopicID))
	}

	if got := rig.api.iconOf(first.TopicID); got != iconOn {
		t.Fatalf("значок вернувшейся в режим сессии: %q", got)
	}

	if rig.api.nameOf(second.TopicID) != topicAlpha {
		t.Fatalf("новую сессию тронули: %q", rig.api.nameOf(second.TopicID))
	}
}

// TestReturnToTakenNumberTakesNextNumber: занят и свой номер — номер подбирается заново, а не
// наращивается вторым хвостом.
func TestReturnToTakenNumberTakesNextNumber(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.mod("s1", topicAlpha)
	conn, _ := rig.mod("s2", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))

	if _, on := rig.mod("s3", topicAlpha); on.TopicName != "alpha#2" {
		t.Fatalf("освободившийся номер не занят: %+v", on)
	}

	if _, back := rig.hello("s2", protocol.RoleSession, protocol.KindMod); back.TopicName != "alpha#3" {
		t.Fatalf("возврат не тот: %+v", back)
	}
}

// TestWordInClosedTopic: слово в закрытую тему — прежнее поведение: 😴, строка об адресате, и слово
// ждёт в ящике вернувшуюся сессию. Служба это помнит и после перезапуска.
func TestWordInClosedTopic(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))
	rig.reopen()

	id := rig.ownerMessage(on.TopicID, "ты тут")

	if got := rig.api.mark(id); got != botapi.MarkHeld {
		t.Fatalf("отметка на слове: %q", got)
	}

	lastLine(t, rig, on.TopicID, "Сессии этой темы в режиме нет.")

	back, _ := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if got := rig.waitDelivery(back); got.Text != "ты тут" {
		t.Fatalf("слово не уехало вернувшейся сессии: %+v", got)
	}
}

// TestHereInClosedTopicTakesBackMode: `/here` снимает режим и у кончившейся сессии: вернётся она без
// него. Тема при этом остаётся закрытой.
func TestHereInClosedTopicTakesBackMode(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))

	rig.ownerMessage(on.TopicID, "/here")

	if !rig.api.closedOf(on.TopicID) || rig.api.iconOf(on.TopicID) != iconOff {
		t.Fatal("`/here` тронул закрытую тему")
	}

	if _, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod); back.Mode != protocol.ModeOff {
		t.Fatalf("сессия вернулась с режимом, снятым словом владельца: %+v", back)
	}
}

// TestAwayInClosedTopicBringsItBack: `/away` работает и у кончившейся сессии: тема снова открыта,
// имя возвращено или занято суффиксом, режим включён.
func TestAwayInClosedTopicBringsItBack(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))
	rig.mod("s2", topicAlpha)

	rig.ownerMessage(on.TopicID, "/away")

	if rig.api.closedOf(on.TopicID) || rig.api.nameOf(on.TopicID) != "alpha#2" {
		t.Fatalf("тема не вернулась: %q", rig.api.nameOf(on.TopicID))
	}

	if got := rig.api.iconOf(on.TopicID); got != iconOn {
		t.Fatalf("значок темы, вернувшейся словом `/away`: %q", got)
	}

	if _, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod); back.Mode != protocol.ModeOn ||
		back.TopicName != "alpha#2" {
		t.Fatalf("возврат не тот: %+v", back)
	}
}

// TestNameReportOfBuriedLeavesTopic: название чата, опоздавшее за `bye`, закрытую тему не правит, но
// запоминается: с ним сессия и вернётся.
func TestNameReportOfBuriedLeavesTopic(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", "")
	rig.do(conn, rig.req(protocol.OpBye))

	held := rig.api.nameOf(on.TopicID)

	req := rig.req(protocol.OpNameReport)
	req.Name = topicBeta

	if resp := rig.do(conn, req); !resp.OK {
		t.Fatalf("название отбито: %+v", resp.Error)
	}

	if got := rig.api.nameOf(on.TopicID); got != held {
		t.Fatalf("закрытая тема переименована: %q", got)
	}

	if rig.sessionRecord("s1").AutoName != topicBeta {
		t.Fatal("название чата не запомнено")
	}
}

// TestBurialRefusalsAreJournalled: отказ платформы на значок или закрытие ложится в журнал, а
// погребение не откатывается: имя свободно, возврат идёт как обычно.
func TestBurialRefusalsAreJournalled(t *testing.T) {
	t.Parallel()

	refusal := botapi.Refuse(protocol.CodeTelegramRefused, "not enough rights to manage topics")

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	rig.api.refuseIcon(1)
	rig.api.fail(methodCloseTopic, refusal)

	if resp := rig.do(conn, rig.req(protocol.OpBye)); !resp.OK {
		t.Fatalf("отказ платформы отбил `bye`: %+v", resp.Error)
	}

	for _, event := range []string{"topic-icon-failed", "topic-close-failed"} {
		if !rig.journalled(event, protocol.CodeTelegramRefused) {
			t.Fatalf("в журнале нет %s: %+v", event, rig.journalEvents())
		}
	}

	if _, second := rig.mod("s2", topicAlpha); second.TopicName != topicAlpha {
		t.Fatalf("имя не свободно: %+v", second)
	}

	rig.api.fail(methodReopenTopic, refusal)
	rig.api.fail(methodRenameTopic, refusal)

	_, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.OK || !back.Revived || back.Mode != protocol.ModeOn || back.TopicID != on.TopicID {
		t.Fatalf("отказ платформы сломал возврат: %+v", back)
	}

	for _, event := range []string{"topic-reopen-failed", "topic-rename-failed"} {
		if !rig.journalled(event, protocol.CodeTelegramRefused) {
			t.Fatalf("в журнале нет %s: %+v", event, rig.journalEvents())
		}
	}
}

// TestIconRefusalStillCloses: отказ значка закрытию не мешает, а значок становится неизвестным:
// исход отказа мог быть любым, и возврат без режима ставит значок выкл ещё раз.
func TestIconRefusalStillCloses(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpModeOff))

	rig.topicEdited(on.TopicID, botapi.TopicEdit{IconChanged: true, Icon: "5312536423851630001"})
	rig.api.refuseIcon(1)
	rig.do(conn, rig.req(protocol.OpBye))

	if !rig.api.closedOf(on.TopicID) {
		t.Fatal("отказ значка отменил закрытие")
	}

	if entry := registryEntry(t, rig, on.TopicID); !entry.IconUnknown {
		t.Fatalf("отбитый значок не назван неизвестным: %+v", entry)
	}

	rig.hello("s1", protocol.RoleSession, protocol.KindMod)

	if got := rig.api.iconOf(on.TopicID); got != iconOff {
		t.Fatalf("значок вернувшейся без режима сессии: %q", got)
	}
}

// TestDeletedTopicAtBurial: тема удалена руками — погребение заново её не заводит и не падает, а
// возврат заводит её тем же путём, что и всякую пропавшую тему.
func TestDeletedTopicAtBurial(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.api.plan(on.TopicID, 100)

	if resp := rig.do(conn, rig.req(protocol.OpBye)); !resp.OK {
		t.Fatalf("удалённая тема отбила `bye`: %+v", resp.Error)
	}

	if names := rig.api.createdNames(); len(names) != 1 {
		t.Fatalf("погребение завело тему заново: %v", names)
	}

	if !rig.journalled("topic-gone", protocol.CodeNoTopic) {
		t.Fatalf("пропажа темы не легла в журнал: %+v", rig.journalEvents())
	}

	_, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.OK || back.Mode != protocol.ModeOn || back.TopicID == on.TopicID || back.TopicName != topicAlpha {
		t.Fatalf("возврат к удалённой теме не тот: %+v", back)
	}

	if names := rig.api.createdNames(); len(names) != 2 || names[1] != topicAlpha {
		t.Fatalf("тема не заведена заново: %v", names)
	}
}

// TestDeletedTopicWithoutModeAtBurial: значка звать незачем, и пропажу темы называет закрытие.
func TestDeletedTopicWithoutModeAtBurial(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpModeOff))
	rig.api.plan(on.TopicID, 100)

	rig.do(conn, rig.req(protocol.OpBye))

	if names := rig.api.createdNames(); len(names) != 1 {
		t.Fatalf("погребение завело тему заново: %v", names)
	}

	if !rig.journalled("topic-gone", protocol.CodeNoTopic) {
		t.Fatalf("пропажа темы не легла в журнал: %+v", rig.journalEvents())
	}
}

// TestNewTopicWithoutModeGetsOffIcon: тема, заведённая заново при снятом режиме, носит значок выкл.
func TestNewTopicWithoutModeGetsOffIcon(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpModeOff))
	rig.do(conn, rig.req(protocol.OpBye))

	rig.api.plan(on.TopicID, 100)

	_, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.OK || back.TopicID == on.TopicID || back.Mode != protocol.ModeOff {
		t.Fatalf("возврат к удалённой теме не тот: %+v", back)
	}

	if got := rig.api.iconOf(back.TopicID); got != iconOff {
		t.Fatalf("значок новой темы без режима: %q", got)
	}
}

// TestDeletedTopicAtReturn: тема удалена уже после закрытия — возврат заводит её заново, и занятое
// к этому времени имя получает суффикс, а реестр не падает на чужой записи.
func TestDeletedTopicAtReturn(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))
	rig.mod("s2", topicAlpha)

	rig.api.plan(on.TopicID, 100)

	_, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.OK || back.TopicID == on.TopicID || back.TopicName != "alpha#2" {
		t.Fatalf("возврат к удалённой теме не тот: %+v", back)
	}
}

// TestLegacyBuriedTopicUntouched: тема сессии, погребённой прежней службой, остаётся какой была:
// служба её не закрывает ни при подъёме, ни при возврате, и освобождает прежним правилом.
func TestLegacyBuriedTopicUntouched(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicAlpha)
	rig.legacyBury("s1")
	rig.reap()

	if rig.api.closedOf(on.TopicID) || rig.api.nameOf(on.TopicID) != topicAlpha {
		t.Fatalf("старую тему тронули: %q", rig.api.nameOf(on.TopicID))
	}

	renames := len(rig.api.renamedNames())

	_, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.Revived || back.Mode != protocol.ModeOn || back.TopicID != on.TopicID {
		t.Fatalf("возврат старой сессии не тот: %+v", back)
	}

	if len(rig.api.renamedNames()) != renames || rig.api.nameOf(on.TopicID) != topicAlpha {
		t.Fatal("возврат старой сессии правил тему")
	}
}

// TestBuryIgnoresRequestCancel: `bye` приходит на выходе, и соединение может оборваться раньше, чем
// платформа ответит: отмена запроса погребение не обрывает.
func TestBuryIgnoresRequestCancel(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rig.doCtx(ctx, conn, rig.req(protocol.OpBye))

	if !rig.api.closedOf(on.TopicID) {
		t.Fatal("отмена запроса оборвала закрытие темы")
	}
}
