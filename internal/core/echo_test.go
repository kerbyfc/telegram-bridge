package core_test

import (
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// eventMessageDropped — строка журнала о сообщении темы, которое служба не взяла входом; почему —
// называет её исход.
const eventMessageDropped = "message-dropped"

// TestOwnEchoSaysNothing: своё же сообщение возвращается обновлением опроса, и служба его не
// разбирает вовсе — это граница входа. Иначе каждое своё сообщение рождало бы в теме строку о
// отброшенном сообщении бота.
func TestOwnEchoSaysNothing(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicAlpha)

	before := len(rig.api.texts(on.TopicID))
	rig.selfMessage(on.TopicID, "строка состояния")

	if after := len(rig.api.texts(on.TopicID)); after != before {
		t.Fatalf("служба заговорила о своём же обновлении: %v", rig.api.texts(on.TopicID)[before:])
	}

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("своё обновление попало в ящик: %v", files)
	}
}

// TestForeignTopicStaysSilent: бот получает обновления всех тем супергруппы, и в теме, которую
// служба не ведёт, она не говорит ничего.
func TestForeignTopicStaysSilent(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicAlpha)

	foreign := on.TopicID + 100
	rig.strangerMessage(foreign, "обсуждение в соседней теме")

	if texts := rig.api.texts(foreign); len(texts) != 0 {
		t.Fatalf("служба заговорила в чужой теме: %v", texts)
	}
}

// TestStrangerIsInvisible: чужое сообщение в сессию не доходит и в теме не называется — в группе
// должен быть только владелец, и чужое сообщение для темы просто невидимо. След — строка журнала.
func TestStrangerIsInvisible(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicAlpha)

	before := len(rig.api.texts(on.TopicID))
	rig.strangerMessage(on.TopicID, "я тут решу")

	if after := len(rig.api.texts(on.TopicID)); after != before {
		t.Fatalf("служба назвала чужое сообщение в теме: %v", rig.api.texts(on.TopicID)[before:])
	}

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("чужое сообщение попало в ящик: %v", files)
	}

	dropped := false

	for _, entry := range rig.journalEvents() {
		if entry.Event == eventMessageDropped && entry.Outcome == "not-owner" {
			dropped = true
		}
	}

	if !dropped {
		t.Fatal("отброшенное чужое сообщение не оставило следа в журнале")
	}
}

// TestOwnerMessageWithoutTextIsNotInput: сообщение владельца без текста — стикер, фото без подписи,
// служебное «тема закрыта» — не вход. В ящик оно не ложится, моду не отдаётся, отметки на нём нет и
// строки в теме тоже; след — строка журнала, как у анонимного слова. Иначе пустую доставку мод не
// принял бы, и запись висела бы в ящике, отдаваясь заново на каждом подключении.
func TestOwnerMessageWithoutTextIsNotInput(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicAlpha)
	before := len(rig.api.texts(on.TopicID))

	// Стикер, фото без подписи и служебное сообщение о закрытии темы приходят от клиента Bot API
	// одинаково: текста у них нет.
	sticker := rig.ownerMessage(on.TopicID, "")
	photo := rig.ownerMessage(on.TopicID, "")

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("сообщение без текста легло в ящик: %v", files)
	}

	for _, id := range []int{sticker, photo} {
		if mark := rig.api.mark(id); mark != "" {
			t.Fatalf("на сообщении без текста отметка %q: его приняли как слово", mark)
		}
	}

	if after := len(rig.api.texts(on.TopicID)); after != before {
		t.Fatalf("служба ответила в тему на сообщение без текста: %v", rig.api.texts(on.TopicID)[before:])
	}

	dropped := 0

	for _, entry := range rig.journalEvents() {
		if entry.Event == eventMessageDropped && entry.Outcome == "no-text" && entry.TopicID == on.TopicID {
			dropped++
		}
	}

	if dropped != 2 {
		t.Fatalf("сообщений без текста два, а следов в журнале %d", dropped)
	}
}

// TestAnonymousWordIsInvisible: сообщение без автора — анонимный админ, сообщение от имени группы —
// подтвердить, что писал владелец, нечем: в сессию оно не доходит, в теме о нём не говорится
// ничего, а след остаётся строкой журнала. Автор у такого обновления — нулевой указатель, и разбор
// на нём не падает.
func TestAnonymousWordIsInvisible(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	before := len(rig.api.texts(on.TopicID))
	rig.core.Dispatch(t.Context(), botapi.Update{
		ID: 3,
		Message: &botapi.Message{
			ID: 4321, TopicID: on.TopicID, Text: "от имени группы", From: nil, At: time.Now(), TopicEdit: nil,
		},
	})

	if after := len(rig.api.texts(on.TopicID)); after != before {
		t.Fatalf("служба заговорила о сообщении без автора: %v", rig.api.texts(on.TopicID)[before:])
	}

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("сообщение без автора попало в ящик: %v", files)
	}

	if got := rig.waitDelivery(mod); got.OK {
		t.Fatalf("мод получил сообщение без автора: %+v", got)
	}

	if mark := rig.api.mark(4321); mark != "" {
		t.Fatalf("сообщение без автора получило отметку доставки: %q", mark)
	}

	dropped := false

	for _, entry := range rig.journalEvents() {
		if entry.Event == eventMessageDropped && entry.Outcome == "anonymous" {
			dropped = true
		}
	}

	if !dropped {
		t.Fatal("сообщение без автора не оставило следа в журнале")
	}
}

// TestModeOffKeepsWordFromMod: со снятым режимом слово из темы сессии не адресовано вовсе —
// подключённый мод его не получает, честь на сообщении не встаёт, а владелец видит причину
// строкой в теме. Запись при этом в ящике остаётся: его слово не потеряно.
func TestModeOffKeepsWordFromMod(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)
	rig.do(mod, rig.req(protocol.OpModeOff))

	before := len(rig.api.texts(on.TopicID))
	ownerID := rig.ownerMessage(on.TopicID, "сделай вот это")

	if files := rig.inboxFiles("s1"); len(files) != 1 {
		t.Fatalf("слово владельца не удержано в ящике: %v", files)
	}

	if after := len(rig.api.texts(on.TopicID)); after == before {
		t.Fatal("владельцу не сказано, что сессии в режиме нет")
	}

	// Моду не пришло ничего: долгий запрос кончается своим сроком, а не доставкой.
	if got := rig.waitDelivery(mod); got.OK {
		t.Fatalf("мод получил слово при снятом режиме: %+v", got)
	}

	if got := rig.api.mark(ownerID); got != botapi.MarkHeld {
		t.Fatalf("слово не отмечено придержанным: %q", got)
	}

	// Режим вернули — слово из ящика уезжает моду сразу.
	on2 := rig.req(protocol.OpModeOn)
	on2.Name = topicAlpha

	if resp := rig.do(mod, on2); !resp.OK {
		t.Fatalf("режим не вернулся: %+v", resp.Error)
	}

	back := rig.waitDelivery(mod)
	if !back.OK || back.Text != "сделай вот это" {
		t.Fatalf("слово владельца не уехало моду после возврата режима: %+v", back)
	}
}

// TestModeOffHoldsWordInInbox: режим снят, а неподтверждённое слово лежит в ящике — у агента его
// нет, и владелец читает это отметкой на своём сообщении, без строк в тему.
func TestModeOffHoldsWordInInbox(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	ownerID := rig.ownerMessage(on.TopicID, "посмотри")
	rig.waitDelivery(mod)

	if mark := rig.api.mark(ownerID); mark != botapi.MarkReceived {
		t.Fatalf("отметка до снятия режима не та: %q", mark)
	}

	before := len(rig.api.texts(on.TopicID))

	if resp := rig.do(mod, rig.req(protocol.OpModeOff)); !resp.OK {
		t.Fatalf("режим не снят: %+v", resp.Error)
	}

	if mark := rig.api.mark(ownerID); mark != botapi.MarkHeld {
		t.Fatalf("снятый режим не отметил слово придержанным: %q", mark)
	}

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("служба заговорила о неподанном слове в теме: %v", texts[before:])
	}
}

// TestLostConnHoldsWord: соединение сессии пропало до подтверждения — слово владельца осталось в
// ящике и отмечено придержанным. Строки в тему при этом нет ни одной: состояние своего слова
// владелец читает на самом сообщении.
func TestLostConnHoldsWord(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	ownerID := rig.ownerMessage(on.TopicID, "посмотри")
	rig.waitDelivery(mod)

	before := len(rig.api.texts(on.TopicID))
	rig.closeConn(mod)

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("служба заговорила о неподанном слове в теме: %v", texts[before:])
	}

	if got := rig.api.mark(ownerID); got != botapi.MarkHeld {
		t.Fatalf("слово не отмечено придержанным: %q", got)
	}

	if files := rig.inboxFiles("s1"); len(files) != 1 {
		t.Fatalf("слово владельца потеряно вместе с соединением: %v", files)
	}
}

// TestLostConnKeepsAcceptedMark: придерживать нечего — принятое слово остаётся с честью, и обрыв
// соединения ни отметки не меняет, ни строки в тему не добавляет.
func TestLostConnKeepsAcceptedMark(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	ownerID := rig.ownerMessage(on.TopicID, "посмотри")
	got := rig.waitDelivery(mod)
	rig.ack(mod, got.DeliveryID, protocol.AckAccepted, "")

	before := len(rig.api.texts(on.TopicID))
	rig.closeConn(mod)

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("служба заговорила об обрыве на пустом ящике: %v", texts[before:])
	}

	if mark := rig.api.mark(ownerID); mark != botapi.MarkAccepted {
		t.Fatalf("честь на принятом слове сменилась обрывом: %q", mark)
	}
}
