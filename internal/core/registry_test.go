package core_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/legacy"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// TestRenameToTakenNameGetsSuffix: имя, которое в реестре держит другая тема, при переименовании
// получает наименьший свободный суффикс. Запись чужой темы не затирается: иначе живая тема
// осталась бы без записи, а её id продолжал бы смотреть на отданное имя, и реестр начал бы
// отдавать одну тему вместо другой.
func TestRenameToTakenNameGetsSuffix(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, first := rig.mod("s1", topicAlpha)
	second, _ := rig.mod("s2", topicBeta)

	req := rig.req(protocol.OpModeOn)
	req.Name = topicAlpha
	renamed := rig.do(second, req)

	if !renamed.OK || renamed.TopicName != topicAlpha+"#2" {
		t.Fatalf("переименование не взяло свободный суффикс: %+v", renamed)
	}

	topics := readTopics(t, rig.root)
	if len(topics.ByName) != 2 || len(topics.ByID) != 2 {
		t.Fatalf("запись живой темы пропала из реестра: %+v", topics)
	}

	entry, ok := topics.ByIDLookup(first.TopicID)
	if !ok || entry.Name != topicAlpha {
		t.Fatalf("прежняя тема потеряла своё имя: %+v", entry)
	}
}

// TestRepeatModeOnWithTakenNameKeepsTopic: сессия держит `alpha#2`, потому что `alpha` занята
// другой темой, и включает режим с `alpha` ещё раз. Подбор суффикса даёт то же `alpha#2`, и
// переименования нет вовсе: в то же имя Telegram переименование отбивает, и включение режима
// упало бы на теме, которая уже называется как надо.
func TestRepeatModeOnWithTakenNameKeepsTopic(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, _ = rig.mod("s1", topicAlpha)
	second, _ := rig.mod("s2", topicBeta)

	req := rig.req(protocol.OpModeOn)
	req.Name = topicAlpha

	first := rig.do(second, req)
	if !first.OK || first.TopicName != topicAlpha+"#2" {
		t.Fatalf("переименование не взяло свободный суффикс: %+v", first)
	}

	renames := len(rig.api.renamedNames())

	again := rig.do(second, req)
	if !again.OK || again.TopicName != topicAlpha+"#2" || again.TopicID != first.TopicID {
		t.Fatalf("повторное включение с занятым именем не прошло: %+v", again)
	}

	if names := rig.api.renamedNames(); len(names) != renames {
		t.Fatalf("тема переименована в то же имя: %v", names[renames:])
	}
}

// TestIconSwitchesOnlyOnChange: значок переключается только на изменение — тот же значок был бы
// лишним вызовом платформы на каждом включении режима.
func TestIconSwitchesOnlyOnChange(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, _ := rig.mod("s1", topicAlpha)

	req := rig.req(protocol.OpModeOn)
	req.Name = topicAlpha
	rig.do(conn, req)
	rig.do(conn, req)

	noJournalEvent(t, rig, "topic-icon-failed")

	// Значок уже стоит, и на лишний вызов клиент Bot API ответил бы успехом «уже так»: лишний
	// вызов видно только по их счёту.
	if calls := rig.api.iconCallCount(); calls != 1 {
		t.Fatalf("вызовов значка %d на трёх включениях режима: значок переключается не только на изменение", calls)
	}
}

// TestHealedTopicSetsIconOnce: лечение пропавшей темы само ставит ей значок, и повтор того же
// действия после лечения не делается.
func TestHealedTopicSetsIconOnce(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	// Снятие режима идёт по пропавшей теме: лечение заведёт её заново и поставит значок.
	rig.api.plan(on.TopicID, 1)
	rig.do(conn, rig.req(protocol.OpModeOff))
	noJournalEvent(t, rig, "topic-icon-failed")

	if names := rig.api.createdNames(); len(names) != 2 {
		t.Fatalf("тема не заведена заново: %v", names)
	}

	// Вызовов значка два: включение режима и снятие, отбитое пропавшей темой. Повтор снятия
	// после лечения был бы третьим.
	if calls := rig.api.iconCallCount(); calls != 2 {
		t.Fatalf("вызовов значка %d, а их два: значок после лечения поставлен ещё раз", calls)
	}
}

// TestHandRemovedIconIsSetAgain: значок, снятый владельцем руками, приходит служебным сообщением
// о правке темы, и реестр его подхватывает. Следующее включение режима ставит значок снова, а не
// пропускает его как «уже так». Правка темы — не слово владельца: сессии она не уезжает.
func TestHandRemovedIconIsSetAgain(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	removed := botapi.TopicEdit{IconChanged: true, Icon: ""}
	rig.topicEdited(on.TopicID, removed)
	// Повтор той же правки ничего не портит: запись приходит в то же состояние.
	rig.topicEdited(on.TopicID, removed)

	if rig.api.inModeOf(on.TopicID) {
		t.Fatal("стенд не снял значок руками")
	}

	req := rig.req(protocol.OpModeOn)
	req.Name = topicAlpha
	rig.do(conn, req)

	if !rig.api.inModeOf(on.TopicID) {
		t.Fatal("значок, снятый руками, не поставлен снова: переключение пропущено как «уже так»")
	}

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("правка темы уехала сессии словом владельца: %v", files)
	}

	noJournalEvent(t, rig, "topic-icon-failed")
}

// TestHandSetIconIsClearedOnModeOff: значок, поставленный руками при снятом режиме, по id эмодзи
// от значка режима не отличить, и запись реестра о значке становится неизвестной. Снятие режима
// после этого уходит платформе, а не пропускается: значок приходит в то состояние, что держит
// служба.
func TestHandSetIconIsClearedOnModeOff(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpModeOff))

	rig.topicEdited(on.TopicID, botapi.TopicEdit{IconChanged: true, Icon: "5312536423851630001"})

	entry := registryEntry(t, rig, on.TopicID)
	if !entry.IconUnknown {
		t.Fatalf("значок, поставленный руками, не назван неизвестным: %+v", entry)
	}

	rig.do(conn, rig.req(protocol.OpModeOff))

	if rig.api.inModeOf(on.TopicID) {
		t.Fatal("значок, поставленный руками, не снят: переключение пропущено как «уже так»")
	}

	if entry = registryEntry(t, rig, on.TopicID); entry.IconUnknown || entry.InMode {
		t.Fatalf("после снятия значок в реестре не известен: %+v", entry)
	}

	noJournalEvent(t, rig, "topic-icon-failed")
}

// TestTopicEditWithoutIconKeepsRegistry: правка темы, не трогавшая значка (имя), реестра значков
// не меняет и сессии словом владельца не уезжает.
func TestTopicEditWithoutIconKeepsRegistry(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicAlpha)
	before := registryEntry(t, rig, on.TopicID)

	rig.topicEdited(on.TopicID, botapi.TopicEdit{IconChanged: false, Icon: ""})

	if after := registryEntry(t, rig, on.TopicID); after != before {
		t.Fatalf("правка без значка тронула запись реестра: %+v → %+v", before, after)
	}

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("правка темы уехала сессии словом владельца: %v", files)
	}
}

// TestRefusedIconSwitchMakesIconUnknown: отказ переключения значка мог быть сетевым — Telegram
// значок снял, а ответ потерялся. Поэтому после отказа следующее переключение уходит платформе и
// тогда, когда прежняя запись реестра считает его «уже так»: включение после отбитого снятия.
func TestRefusedIconSwitchMakesIconUnknown(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	rig.api.refuseIcon(1)
	rig.do(conn, rig.req(protocol.OpModeOff))

	calls := rig.api.iconCallCount()

	req := rig.req(protocol.OpModeOn)
	req.Name = topicAlpha
	rig.do(conn, req)

	if got := rig.api.iconCallCount(); got != calls+1 {
		t.Fatalf("включение после отбитого снятия пропущено по прежней записи: вызовов %d, было %d", got, calls)
	}

	if !rig.api.inModeOf(on.TopicID) {
		t.Fatal("значок режима не стоит после включения")
	}
}

// TestPortedTopicIconGoesToPlatform: тема, перенесённая со старого моста, приходит в реестр со
// значком «неизвестен». Первое включение режима зовёт значок, и даже когда платформа его отбила,
// первое снятие зовёт значок тоже: с «значок снят» снятие пропустилось бы как «уже так», и значок,
// оставленный старым мостом или рукой владельца, так бы и стоял.
func TestPortedTopicIconGoesToPlatform(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	portTopic(t, rig, topicPorted, portedID)
	rig.reopen()

	rig.api.refuseIcon(1)

	conn, on := rig.mod("s1", topicPorted)
	if on.TopicID != portedID {
		t.Fatalf("перенесённая тема не взята по имени: %+v", on)
	}

	rig.do(conn, rig.req(protocol.OpModeOff))

	if calls := rig.api.iconCallCount(); calls != 2 {
		t.Fatalf("вызовов значка %d, а их два: снятие после отбитого включения пропущено как «уже так»", calls)
	}

	if entry := registryEntry(t, rig, portedID); entry.IconUnknown || entry.InMode {
		t.Fatalf("после снятия значок перенесённой темы не известен: %+v", entry)
	}
}

// TestNearOwnCommandGoesToModAsClientCommand: своих команд у службы две, `/away` и `/here`, и
// опознаются они точным именем. Похожее слово — опечатка или чужая команда — службой как её
// команда не исполняется и не теряется: оно уезжает моду командой клиента, и неизвестную команду
// назовёт сам клиент. Подсказки похожей команды служба не даёт, и в теме о ней ни строки.
func TestNearOwnCommandGoesToModAsClientCommand(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)
	before := len(rig.api.texts(on.TopicID))

	for _, text := range []string{"/awayyyyy", "/away-now", "/heres", "/status", "/statis"} {
		rig.ownerMessage(on.TopicID, text)

		got := rig.waitDelivery(mod)
		if !got.OK || got.Kind != protocol.DeliveryClientCmd || got.Text != text {
			t.Fatalf("%s: не уехало моду командой клиента: %+v", text, got)
		}

		rig.ack(mod, got.DeliveryID, protocol.AckAccepted, "")

		status := rig.do(mod, rig.req(protocol.OpModeStatus))
		if status.Mode != protocol.ModeOn {
			t.Fatalf("%s: служба исполнила похожее слово своей командой: %+v", text, status)
		}
	}

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("служба заговорила в теме о похожей команде: %v", texts[before:])
	}
}

// TestOwnCommandTakesBotNameAndArgs: команда службы опознаётся первым словом без суффикса
// `@<бот>` — в группе с несколькими ботами клиент Telegram подставляет имя бота сам, — а её
// аргумент становится именем темы.
func TestOwnCommandTakesBotNameAndArgs(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	rig.ownerMessage(on.TopicID, "/here@telegram_bridge_bot")

	status := rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.Mode != protocol.ModeOff {
		t.Fatalf("команда с именем бота не опознана: %+v", status)
	}

	rig.ownerMessage(on.TopicID, "/away@telegram_bridge_bot "+topicBeta)

	status = rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.Mode != protocol.ModeOn || status.TopicName != topicBeta {
		t.Fatalf("аргумент команды не стал именем темы: %+v", status)
	}

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("команда службы уехала моду как команда клиента: %v", files)
	}

	if !rig.api.inModeOf(status.TopicID) {
		t.Fatal("значок темы не переключён в режим командой владельца")
	}
}

// Тема старого моста на стенде: имя и id, под которыми её переносит `import-topics`.
const (
	topicPorted = "перенос"
	portedID    = 4242
)

// portTopic переносит тему старого моста в состояние стенда тем же путём, что `import-topics`:
// реестр старого моста читается с диска и кладётся в реестр службы.
func portTopic(t *testing.T, rig *rig, name string, id int64) {
	t.Helper()

	path := filepath.Join(t.TempDir(), legacy.TopicsFile)

	err := os.WriteFile(path, []byte(fmt.Sprintf(`{%q: %d}`, name, id)), 0o600)
	if err != nil {
		t.Fatalf("реестр старого моста не записан: %v", err)
	}

	registry, err := legacy.ReadTopics(path)
	if err != nil {
		t.Fatalf("реестр старого моста не прочитан: %v", err)
	}

	topics, err := store.LoadTopics(rig.store)
	if err != nil {
		t.Fatalf("реестр стенда не прочитан: %v", err)
	}

	if report := registry.Port(topics); report.Placed != 1 {
		t.Fatalf("тема не перенесена: %+v", report)
	}

	err = topics.Save(rig.store)
	if err != nil {
		t.Fatalf("реестр стенда не записан: %v", err)
	}
}

// readTopics читает реестр тем с диска.
func readTopics(t *testing.T, root string) store.Topics {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(root, "topics.json"))
	if err != nil {
		t.Fatalf("реестр не прочитан: %v", err)
	}

	var topics store.Topics

	err = json.Unmarshal(body, &topics)
	if err != nil {
		t.Fatalf("реестр не разобран: %v", err)
	}

	return topics
}

// noJournalEvent отбивает тест, если такое событие в журнале есть.
func noJournalEvent(t *testing.T, rig *rig, event string) {
	t.Helper()

	for _, entry := range rig.journalEvents() {
		if entry.Event == event {
			t.Fatalf("в журнале есть %q: %+v", event, entry)
		}
	}
}
