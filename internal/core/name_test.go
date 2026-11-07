package core_test

import (
	"strings"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// chat — название чата, которое средство сессии называет службе в тестах.
const chat = "Проверка моста"

// TestModeOnWithoutNameTakesChatName: имя, названное средством сессии, — это имя темы для
// включения режима без аргумента. Владельцу не надо набирать его руками.
func TestModeOnWithoutNameTakesChatName(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	conn, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("hello мода отказал: %+v", resp.Error)
	}

	report := rig.req(protocol.OpNameReport)
	report.Name = chat

	if got := rig.do(conn, report); !got.OK {
		t.Fatalf("имя чата не принято: %+v", got.Error)
	}

	on := rig.do(conn, rig.req(protocol.OpModeOn))

	if !on.OK || !on.Created || on.TopicName != chat {
		t.Fatalf("тема заведена не по названию чата: %+v", on)
	}

	if names := rig.api.createdNames(); len(names) != 1 || names[0] != chat {
		t.Fatalf("тем заведено не по названию чата: %v", names)
	}
}

// TestChatNameRenamesBoundTopic: название чата поменялось, пока тема привязана, — тема
// переименовывается тем же путём, что и слово владельца, и новой темы не заводится.
func TestChatNameRenamesBoundTopic(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	conn, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("hello мода отказал: %+v", resp.Error)
	}

	first := rig.req(protocol.OpNameReport)
	first.Name = chat
	rig.do(conn, first)

	on := rig.do(conn, rig.req(protocol.OpModeOn))

	second := rig.req(protocol.OpNameReport)
	second.Name = topicBeta

	if got := rig.do(conn, second); !got.OK {
		t.Fatalf("новое имя чата не принято: %+v", got.Error)
	}

	if names := rig.api.renamedNames(); len(names) != 1 || names[0] != topicBeta {
		t.Fatalf("тема не переименована за чатом: %v", names)
	}

	status := rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.TopicID != on.TopicID || status.TopicName != topicBeta {
		t.Fatalf("привязка не знает нового имени: %+v", status)
	}

	if names := rig.api.createdNames(); len(names) != 1 {
		t.Fatalf("заведена лишняя тема: %v", names)
	}

	// Имя темы, а не только реестр: вклад в статусную строку правится вместе с ним.
	if got := rig.contribution(); got.Items[0].Parts[0].Text != topicBeta {
		t.Fatalf("в статусной строке прежнее имя: %+v", got.Items[0].Parts)
	}
}

// TestAwayNameBecomesChatName: имя, названное аргументом `/away`, становится названием чата у
// службы: тема получает его, оно запоминается, и включение без аргумента берёт его же.
func TestAwayNameBecomesChatName(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	conn, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("hello мода отказал: %+v", resp.Error)
	}

	report := rig.req(protocol.OpNameReport)
	report.Name = chat
	rig.do(conn, report)
	rig.do(conn, rig.req(protocol.OpModeOn))

	on := rig.req(protocol.OpModeOn)
	on.Name = topicAlpha

	named := rig.do(conn, on)
	if !named.OK || named.TopicName != topicAlpha {
		t.Fatalf("аргумент `/away` не дал теме имени: %+v", named)
	}

	if names := rig.api.renamedNames(); len(names) != 1 || names[0] != topicAlpha {
		t.Fatalf("тема не переименована аргументом: %v", names)
	}

	if got := rig.sessionRecord("s1").AutoName; got != topicAlpha {
		t.Fatalf("аргумент не запомнен названием чата: %q", got)
	}

	again := rig.do(conn, rig.req(protocol.OpModeOn))
	if again.TopicID != named.TopicID || again.TopicName != topicAlpha {
		t.Fatalf("включение без аргумента не взяло названное имя: %+v", again)
	}
}

// TestChatNameRenamesAfterAwayName: аргумент `/away` не главнее названия чата — следующее название
// от средства сессии переименовывает тему так же, как без аргумента.
func TestChatNameRenamesAfterAwayName(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	report := rig.req(protocol.OpNameReport)
	report.Name = topicBeta
	rig.do(conn, report)

	if names := rig.api.renamedNames(); len(names) != 1 || names[0] != topicBeta {
		t.Fatalf("тема после аргумента не пошла за чатом: %v", names)
	}

	status := rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.TopicID != on.TopicID || status.TopicName != topicBeta {
		t.Fatalf("привязка не знает нового имени: %+v", status)
	}
}

// TestTopicAwayNameBecomesChatName: аргумент `/away` из темы — то же, что аргумент из сессии:
// тема переименовывается, и имя запоминается названием чата.
func TestTopicAwayNameBecomesChatName(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicAlpha)

	rig.ownerMessage(on.TopicID, "/away "+topicBeta)

	if names := rig.api.renamedNames(); len(names) != 1 || names[0] != topicBeta {
		t.Fatalf("тема не переименована аргументом из темы: %v", names)
	}

	if got := rig.sessionRecord("s1").AutoName; got != topicBeta {
		t.Fatalf("аргумент из темы не запомнен названием чата: %q", got)
	}
}

// TestChatNameSameDoesNotRename: то же название переименования не вызывает — повторный вызов
// отбивает сама платформа.
func TestChatNameSameDoesNotRename(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	conn, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("hello мода отказал: %+v", resp.Error)
	}

	report := rig.req(protocol.OpNameReport)
	report.Name = chat
	rig.do(conn, report)
	rig.do(conn, rig.req(protocol.OpModeOn))
	rig.do(conn, report)

	if names := rig.api.renamedNames(); len(names) != 0 {
		t.Fatalf("переименование в то же имя вызвано: %v", names)
	}
}

// TestChatNameOccupiedTakesSuffix: две живые сессии с одним названием чата — вторая получает тему
// с наименьшим свободным номерным суффиксом, и её имя остаётся таким и при повторном названии.
func TestChatNameOccupiedTakesSuffix(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	first, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("hello первого мода отказал: %+v", resp.Error)
	}

	report := rig.req(protocol.OpNameReport)
	report.Name = chat
	rig.do(first, report)

	one := rig.do(first, rig.req(protocol.OpModeOn))
	if one.TopicName != chat {
		t.Fatalf("первая сессия получила не своё имя: %+v", one)
	}

	second, resp := rig.hello("s2", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("hello второго мода отказал: %+v", resp.Error)
	}

	rig.do(second, report)

	two := rig.do(second, rig.req(protocol.OpModeOn))
	if two.TopicName != chat+"#2" || !two.Created {
		t.Fatalf("вторая сессия не получила суффикс: %+v", two)
	}

	rig.do(second, report)

	if names := rig.api.renamedNames(); len(names) != 0 {
		t.Fatalf("имя с суффиксом переименовано: %v", names)
	}
}

// TestChatNameRenamesTopicWithModeOff: привязка переживает снятие режима, и тема продолжает
// следовать за чатом — имя темы не привязано к значку режима.
func TestChatNameRenamesTopicWithModeOff(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	conn, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("hello мода отказал: %+v", resp.Error)
	}

	report := rig.req(protocol.OpNameReport)
	report.Name = topicAlpha
	rig.do(conn, report)
	rig.do(conn, rig.req(protocol.OpModeOn))
	rig.do(conn, rig.req(protocol.OpModeOff))

	next := rig.req(protocol.OpNameReport)
	next.Name = topicBeta
	rig.do(conn, next)

	if names := rig.api.renamedNames(); len(names) != 1 || names[0] != topicBeta {
		t.Fatalf("тема без режима за чатом не пошла: %v", names)
	}
}

// TestEmptyChatNameKeepsTopicName: у чата названия нет — тема остаётся при прежнем имени, как и
// при включении режима без аргумента.
func TestEmptyChatNameKeepsTopicName(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	conn, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("hello мода отказал: %+v", resp.Error)
	}

	report := rig.req(protocol.OpNameReport)
	report.Name = topicAlpha
	rig.do(conn, report)
	rig.do(conn, rig.req(protocol.OpModeOn))

	empty := rig.req(protocol.OpNameReport)

	if got := rig.do(conn, empty); !got.OK {
		t.Fatalf("пустое имя отбито: %+v", got.Error)
	}

	if names := rig.api.renamedNames(); len(names) != 0 {
		t.Fatalf("тема переименована на пустое имя: %v", names)
	}

	status := rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.TopicName != topicAlpha {
		t.Fatalf("прежнее имя потеряно: %+v", status)
	}
}

// TestChatNameFromToolRefused: имя чата называет соединение сессии; короткий вызов чужое имя не
// называет.
func TestChatNameFromToolRefused(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	tool := rig.tool("s1")

	req := rig.req(protocol.OpNameReport)
	req.Name = topicAlpha

	refused := rig.do(tool, req)
	if refused.OK || refused.Error == nil || refused.Error.Code != protocol.CodeBadRequest {
		t.Fatalf("короткий вызов назвал имя чата: %+v", refused)
	}
}

// TestChatNameSurvivesRestart: название чата живёт в привязке сессии, а не в памяти процесса:
// после перезапуска службы включение без аргумента берёт его же и тему не переименовывает.
func TestChatNameSurvivesRestart(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	conn, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("hello мода отказал: %+v", resp.Error)
	}

	report := rig.req(protocol.OpNameReport)
	report.Name = topicBeta
	rig.do(conn, report)

	rig.reopen()

	if got := rig.sessionRecord("s1").AutoName; got != topicBeta {
		t.Fatalf("после перезапуска название чата потеряно: %q", got)
	}

	again := rig.do(conn, rig.req(protocol.OpModeOn))
	if again.TopicName != topicBeta {
		t.Fatalf("после перезапуска включение не взяло название чата: %+v", again)
	}

	if names := rig.api.renamedNames(); len(names) != 0 {
		t.Fatalf("перезапуск переименовал тему: %v", names)
	}
}

// TestChatNameIsCutAtPlatformLimit: длинное название чата режется пределом платформы до реестра —
// в реестре и в закреплённой строке то же имя, что у темы в Telegram.
func TestChatNameIsCutAtPlatformLimit(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	conn, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("hello мода отказал: %+v", resp.Error)
	}

	long := strings.Repeat("я", 200)

	report := rig.req(protocol.OpNameReport)
	report.Name = strings.Repeat("а", 100)
	rig.do(conn, report)
	rig.do(conn, rig.req(protocol.OpModeOn))

	next := rig.req(protocol.OpNameReport)
	next.Name = long
	rig.do(conn, next)

	status := rig.do(conn, rig.req(protocol.OpModeStatus))

	if status.TopicName != botapi.TopicName(long, "") {
		t.Fatalf("в реестре не то имя, что у темы: %q", status.TopicName)
	}

	if names := rig.api.renamedNames(); len(names) != 1 || names[0] != status.TopicName {
		t.Fatalf("переименование назвало другое имя: %v", names)
	}
}
