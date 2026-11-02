package core_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestOwnerWordGoesByDelivery: путь у слова владельца один — доставка соединению сессии. Запись
// держится в ящике до подтверждения приёма, и только подтверждение ставит вторую отметку.
//
//nolint:cyclop // один путь доставки проверяется от начала до конца: каждый шаг — своя проверка
func TestOwnerWordGoesByDelivery(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	delivered := make(chan protocol.Response, 1)

	go func() { delivered <- rig.waitDelivery(mod) }()

	time.Sleep(20 * time.Millisecond)

	ownerID := rig.ownerMessage(on.TopicID, "посмотри ещё раз")

	resp := <-delivered
	if resp.Kind != protocol.DeliveryOwnerMessage || resp.Text != "посмотри ещё раз" {
		t.Fatalf("доставка не та: %+v", resp)
	}

	if resp.DeliveryID == "" || resp.Provenance == "" || resp.Author == nil {
		t.Fatalf("доставка без id, провенанса или автора: %+v", resp)
	}

	if resp.Topic == nil || resp.Topic.Name != topicAlpha {
		t.Fatalf("тема доставки не названа: %+v", resp.Topic)
	}

	if files := rig.inboxFiles("s1"); len(files) != 1 {
		t.Fatalf("запись снята до подтверждения приёма: %v", files)
	}

	if rig.api.mark(ownerID) != botapi.MarkReceived {
		t.Fatalf("первая отметка не поставлена: %q", rig.api.mark(ownerID))
	}

	rig.ack(mod, resp.DeliveryID, protocol.AckAccepted, "")

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("запись не снята после подтверждения: %v", files)
	}

	if rig.api.mark(ownerID) != botapi.MarkAccepted {
		t.Fatalf("вторая отметка не поставлена: %q", rig.api.mark(ownerID))
	}

	if again := rig.ack(mod, resp.DeliveryID, protocol.AckAccepted, ""); !again.OK {
		t.Fatal("повторное подтверждение не идемпотентно")
	}
}

// TestRejectedKeepsRecord: отказ подачи оставляет запись в ящике, чести не ставит и показывает
// владельцу придержанное слово отметкой, а не строкой в теме. Причина отказа остаётся в журнале
// службы.
func TestRejectedKeepsRecord(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	ownerID := rig.ownerMessage(on.TopicID, "сделай вот это")
	got := rig.waitDelivery(mod)

	before := len(rig.api.texts(on.TopicID))
	rig.ack(mod, got.DeliveryID, protocol.AckRejected, "подача промпта отказала")

	if files := rig.inboxFiles("s1"); len(files) != 1 {
		t.Fatalf("запись снята при отказе приёма: %v", files)
	}

	if mark := rig.api.mark(ownerID); mark != botapi.MarkHeld {
		t.Fatalf("отказ приёма не отметил слово придержанным: %q", mark)
	}

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("служба заговорила об отказе приёма в теме: %v", texts[before:])
	}

	rejected := false

	for _, entry := range rig.journalEvents() {
		if entry.Event == "record-rejected" && entry.Text == "подача промпта отказала" {
			rejected = true
		}
	}

	if !rejected {
		t.Fatal("причина отказа не осталась в журнале службы")
	}
}

// TestDeliveryAfterIsNotAccepted: сдвиг курсора `after` приёмом не считается. Запись остаётся в
// ящике без второй отметки, повторно тому же соединению не подаётся, а следующее слово владельца
// приходит как обычно.
func TestDeliveryAfterIsNotAccepted(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	ownerID := rig.ownerMessage(on.TopicID, "первое")
	one := rig.waitDelivery(mod)

	second := make(chan protocol.Response, 1)

	go func() {
		ctx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()

		req := rig.req(protocol.OpDeliveryWait)
		req.After = one.DeliveryID

		second <- rig.doCtx(ctx, mod, req)
	}()

	time.Sleep(20 * time.Millisecond)

	if files := rig.inboxFiles("s1"); len(files) != 1 {
		t.Fatalf("запись снята сдвигом курсора, без подтверждения приёма: %v", files)
	}

	if rig.api.mark(ownerID) != botapi.MarkReceived {
		t.Fatalf("вторая отметка встала без подтверждения приёма: %q", rig.api.mark(ownerID))
	}

	rig.ownerMessage(on.TopicID, "второе")

	two := <-second
	if two.Text != "второе" {
		t.Fatalf("доставка отдана повторно: %+v", two)
	}

	rig.ack(mod, one.DeliveryID, protocol.AckAccepted, "")

	if rig.api.mark(ownerID) != botapi.MarkAccepted {
		t.Fatalf("подтверждение приёма второй отметки не поставило: %q", rig.api.mark(ownerID))
	}
}

// TestHeldWordGoesToNewConn: приём не подтвердился — запись придержана без второй отметки и тому
// же соединению в цикле не подаётся; подключилась сессия — служба подаёт слово заново, и честь
// встаёт по её подтверждению.
func TestHeldWordGoesToNewConn(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	first, on := rig.mod("s1", topicAlpha)

	ownerID := rig.ownerMessage(on.TopicID, "сделай вот это")
	got := rig.waitDelivery(first)

	rig.ack(first, got.DeliveryID, protocol.AckRejected, "подача промпта отказала")

	held := make(chan protocol.Response, 1)

	go func() {
		ctx, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer stop()

		held <- rig.doCtx(ctx, first, rig.req(protocol.OpDeliveryWait))
	}()

	if again := <-held; again.OK {
		t.Fatalf("придержанное слово подано тому же соединению кругом: %+v", again)
	}

	if mark := rig.api.mark(ownerID); mark != botapi.MarkHeld {
		t.Fatalf("неподтверждённый приём не отметил слово придержанным: %q", mark)
	}

	second, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.OK {
		t.Fatalf("вернувшийся мод отбит: %+v", back.Error)
	}

	mine := rig.waitDelivery(second)
	if mine.Text != "сделай вот это" {
		t.Fatalf("подключившаяся сессия придержанного слова не получила: %+v", mine)
	}

	rig.ack(second, mine.DeliveryID, protocol.AckAccepted, "")

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("запись не снята подтверждением приёма: %v", files)
	}

	if rig.api.mark(ownerID) != botapi.MarkAccepted {
		t.Fatalf("вторая отметка не встала по подтверждению: %q", rig.api.mark(ownerID))
	}
}

// TestClientCommandGoesToMod: всё, кроме своих команд темы, уезжает моду командой клиента — имён
// и смысла команд проекта служба не знает.
func TestClientCommandGoesToMod(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	rig.ownerMessage(on.TopicID, "/compact")
	resp := rig.waitDelivery(mod)

	if resp.Kind != protocol.DeliveryClientCmd || resp.Text != "/compact" {
		t.Fatalf("команда клиента не уехала моду: %+v", resp)
	}
}

// TestDisplacedConnLosesDelivery: новое соединение той же сессии вытесняет прежнее — его долгий
// запрос кончается отказом, а доставку получает новое.
func TestDisplacedConnLosesDelivery(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	first, on := rig.mod("s1", topicAlpha)

	waiting := make(chan protocol.Response, 1)

	go func() { waiting <- rig.waitDelivery(first) }()

	time.Sleep(20 * time.Millisecond)

	second, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.OK {
		t.Fatalf("вернувшийся мод отбит: %+v", back.Error)
	}

	select {
	case resp := <-waiting:
		if resp.OK {
			t.Fatalf("вытесненное соединение получило доставку: %+v", resp)
		}
	case <-time.After(time.Second):
		t.Fatal("вытесненное соединение висит на долгом запросе")
	}

	rig.ownerMessage(on.TopicID, "слово после возврата")

	got := rig.waitDelivery(second)
	if got.Text != "слово после возврата" {
		t.Fatalf("новое соединение доставки не получило: %+v", got)
	}
}

// TestDisplacedConnCannotAck: подтверждает приём то же соединение, что держит сессию. Иначе
// вытесненное прежнее снимало бы из ящика запись, которую получило новое, и ставило бы честь за
// чужой приём.
func TestDisplacedConnCannotAck(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	first, on := rig.mod("s1", topicAlpha)

	ownerID := rig.ownerMessage(on.TopicID, "посмотри")
	got := rig.waitDelivery(first)

	second, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.OK {
		t.Fatalf("вернувшийся мод отбит: %+v", back.Error)
	}

	if resp := rig.ack(first, got.DeliveryID, protocol.AckAccepted, ""); resp.OK {
		t.Fatal("вытесненное соединение подтвердило приём за новое")
	}

	if files := rig.inboxFiles("s1"); len(files) != 1 {
		t.Fatalf("запись снята чужим подтверждением: %v", files)
	}

	if rig.api.mark(ownerID) == botapi.MarkAccepted {
		t.Fatal("честь встала за чужой приём")
	}

	// Новое соединение подтверждает свой приём как обычно.
	mine := rig.waitDelivery(second)
	if !mine.OK {
		t.Fatalf("новое соединение доставки не получило: %+v", mine)
	}

	rig.ack(second, mine.DeliveryID, protocol.AckAccepted, "")

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("запись не снята своим подтверждением: %v", files)
	}
}

// TestDeliveryNeedsSessionConn: доставку берёт только соединение сессии.
func TestDeliveryNeedsSessionConn(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.mod("s1", topicAlpha)

	tool := rig.tool("s1")

	req := rig.req(protocol.OpDeliveryWait)
	req.Session = "s1"

	if resp := rig.do(tool, req); resp.OK {
		t.Fatal("короткий вызов взял доставку сессии")
	}
}

// TestInboxSurvivesRestart: неподтверждённое слово владельца переживает перезапуск службы, и
// вернувшийся мод получает его снова первым же долгим запросом.
func TestInboxSurvivesRestart(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicAlpha)
	rig.ownerMessage(on.TopicID, "слово до перезапуска")

	rig.reopen()

	back, hello := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !hello.OK {
		t.Fatalf("сессия не вернулась: %+v", hello.Error)
	}

	got := rig.waitDelivery(back)
	if got.Text != "слово до перезапуска" {
		t.Fatalf("слово владельца не дожило до возврата мода: %+v", got)
	}
}

// TestVersionMismatchIsLoud: расхождение мажора контракта — отказ с кодом.
func TestVersionMismatchIsLoud(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn := rig.core.NewConn()
	req := rig.req(protocol.OpHello)
	req.V = protocol.Version + 1
	req.Role = protocol.RoleSession
	req.Kind = string(protocol.KindMod)
	req.Session = "s1"

	resp := rig.do(conn, req)
	if resp.OK || resp.Error.Code != protocol.CodeVersion {
		t.Fatalf("расхождение версий не отбито: %+v", resp)
	}
}

// TestUnknownOpIsNamed: операции, которой в этой версии нет, отвечает `unknown-op`.
func TestUnknownOpIsNamed(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, _ := rig.hello("s1", protocol.RoleSession, protocol.KindMod)

	resp := rig.do(conn, rig.req("question.release"))
	if resp.OK || resp.Error.Code != protocol.CodeUnknownOp {
		t.Fatalf("снятая операция не названа неизвестной: %+v", resp)
	}
}

// TestMarkFailureKeepsDelivery: отметка доставки не встала — отказ реакции слово владельца не
// теряет и не удваивает. Запись в ящике одна, мод получает её как обычно, причина ложится строкой
// журнала, а не сообщением в тему, и следующая отметка ставится своим чередом.
func TestMarkFailureKeepsDelivery(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	rig.api.fail(methodSetReaction, botapi.Refuse(protocol.CodeNetwork,
		"setMessageReaction: сеть до Bot API не отвечает"))

	const word = "проверь отметку"

	before := len(rig.api.texts(on.TopicID))
	ownerID := rig.ownerMessage(on.TopicID, word)

	if files := rig.inboxFiles("s1"); len(files) != 1 {
		t.Fatalf("отказ отметки сбил запись ящика: %v", files)
	}

	got := rig.waitDelivery(mod)
	if !got.OK || got.Text != word {
		t.Fatalf("слово владельца не уехало моду после отказа отметки: %+v", got)
	}

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("отказ отметки сказан в теме: %v", texts[before:])
	}

	if count := journalCount(t, rig, "mark-failed"); count != 1 {
		t.Fatalf("строк журнала об отказе отметки %d, а отказ был один", count)
	}

	rig.ack(mod, got.DeliveryID, protocol.AckAccepted, "")

	if mark := rig.api.mark(ownerID); mark != botapi.MarkAccepted {
		t.Fatalf("отметка приёма не встала после отказа первой: %q", mark)
	}
}

// TestHeldWordsKeepOwnerOrderAcrossRestart: придержанные слова уезжают моду в том порядке, в каком
// владелец их писал, — через два перезапуска службы и через смену разрядности номера записи:
// девять слов до первого перезапуска, два после него, одно из них — команда клиента. Номер новой
// записи продолжает последовательность ящика, а не начинается заново: иначе новое слово заняло бы
// номер и файл старого, и второй перезапуск отдал бы слова не в том порядке, потеряв часть.
func TestHeldWordsKeepOwnerOrderAcrossRestart(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)
	rig.closeConn(mod)

	want := make([]string, 0, 11)

	for num := 1; num <= 9; num++ {
		text := fmt.Sprintf("слово %d", num)
		rig.ownerMessage(on.TopicID, text)
		want = append(want, text)
	}

	rig.reopen()

	rig.ownerMessage(on.TopicID, "слово 10")
	rig.ownerMessage(on.TopicID, "/compact")

	want = append(want, "слово 10", "/compact")

	rig.reopen()

	back, hello := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !hello.OK {
		t.Fatalf("сессия не вернулась: %+v", hello.Error)
	}

	for at, text := range want {
		got := rig.waitDelivery(back)
		if !got.OK || got.Text != text {
			t.Fatalf("доставка %d: %+v, а владелец писал %q", at+1, got, text)
		}

		kind := protocol.DeliveryOwnerMessage
		if strings.HasPrefix(text, "/") {
			kind = protocol.DeliveryClientCmd
		}

		if got.Kind != kind {
			t.Fatalf("доставка %d пришла видом %q, а ждали %q", at+1, got.Kind, kind)
		}

		rig.ack(back, got.DeliveryID, protocol.AckAccepted, "")
	}

	if files := rig.inboxFiles("s1"); len(files) != 0 {
		t.Fatalf("в ящике остались записи после приёма всех слов: %v", files)
	}
}
