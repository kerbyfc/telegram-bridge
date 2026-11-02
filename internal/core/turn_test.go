package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestTurnEndIsNotice: конец хода — извещение. Ответа он не ждёт и в тему ничего не публикует:
// ожидания хода у службы нет, ход просто кончается, а сессию будит слово владельца.
func TestTurnEndIsNotice(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	before := len(rig.api.texts(on.TopicID))

	started := time.Now()
	resp := rig.do(mod, rig.req(protocol.OpTurnEnd))
	waited := time.Since(started)

	if !resp.OK {
		t.Fatalf("конец хода отказал: %+v", resp.Error)
	}

	if waited > 100*time.Millisecond {
		t.Fatalf("конец хода ждал ответа: %s", waited)
	}

	if len(rig.api.texts(on.TopicID)) != before {
		t.Fatalf("конец хода что-то опубликовал: %v", rig.api.texts(on.TopicID))
	}

	// Второй конец хода тем же соединением — такое же извещение, без следов «того же хода».
	if second := rig.do(mod, rig.req(protocol.OpTurnEnd)); !second.OK {
		t.Fatalf("второй конец хода отказал: %+v", second.Error)
	}
}

// TestTurnBoundsHoldTyping: на границах хода служба держит действие чата — живость сессии
// владелец видит им, и в истории темы оно не остаётся.
func TestTurnBoundsHoldTyping(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, _ := rig.mod("s1", topicAlpha)

	if rig.api.chatActions() != 0 {
		t.Fatal("действие чата держится до начала хода")
	}

	rig.do(mod, rig.req(protocol.OpTurnStart))

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && rig.api.chatActions() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	if rig.api.chatActions() == 0 {
		t.Fatal("действие чата на ходу не держится")
	}

	rig.do(mod, rig.req(protocol.OpTurnEnd))
}

// TestQuestionAskSendsAndReturns: вопрос с вариантами уходит в тему пронумерованным и отвечает
// сразу — ждать ответа больше нечем, вызов инструмента завершается фактом отправки.
func TestQuestionAskSendsAndReturns(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	ask := rig.req(protocol.OpQuestionAsk)
	ask.Question = "что делать"
	ask.Options = []protocol.Option{{Key: "доделать", Text: "а"}, {Key: "отложить", Text: "б"}}

	started := time.Now()
	resp := rig.do(mod, ask)
	waited := time.Since(started)

	if !resp.OK || resp.MessageID == 0 {
		t.Fatalf("вопрос не отправлен: %+v", resp)
	}

	if waited > 100*time.Millisecond {
		t.Fatalf("вопрос с вариантами ждал ответа: %s", waited)
	}

	texts := rig.api.texts(on.TopicID)
	last := texts[len(texts)-1]

	if !strings.Contains(last, "1) доделать") || !strings.Contains(last, "2) отложить") {
		t.Fatalf("варианты не пронумерованы: %q", last)
	}
}

// TestQuestionAskSliced: вопрос, не поместившийся в одно сообщение, живёт в теме несколькими, и
// все их id возвращаются звонящему.
func TestQuestionAskSliced(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, _ := rig.mod("s1", topicAlpha)

	ask := rig.req(protocol.OpQuestionAsk)
	ask.Question = strings.Repeat("ю", 4500)
	resp := rig.do(mod, ask)

	if len(resp.MessageIDs) != 2 {
		t.Fatalf("вопрос не нарезан: %v", resp.MessageIDs)
	}
}

// TestRepeatedQuestionIsNotSwallowed: вопросы в тему отправляет один канал, и глотать нечем:
// второй вопрос — даже с тем же текстом — уходит в тему своим сообщением, а не сливается с
// первым. Дедупликации по отпечатку текста у службы нет.
func TestRepeatedQuestionIsNotSwallowed(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)
	before := len(rig.api.texts(on.TopicID))

	ask := rig.req(protocol.OpQuestionAsk)
	ask.Question = "как быть?"
	ask.Options = []protocol.Option{{Key: "доделать", Text: ""}, {Key: "отложить", Text: ""}}

	first := rig.do(mod, ask)
	second := rig.do(mod, ask)

	if !first.OK || !second.OK || first.MessageID == second.MessageID {
		t.Fatalf("повторный вопрос не ушёл своим сообщением: %+v, %+v", first, second)
	}

	sent := rig.api.texts(on.TopicID)[before:]
	if len(sent) != 2 || !strings.Contains(sent[0], "как быть?") || !strings.Contains(sent[1], "как быть?") {
		t.Fatalf("в теме не два вопроса: %q", sent)
	}
}

// TestReplyStaysOutOfTopicWithoutMode: со снятым режимом владелец у терминала, и поток реплик в
// тему не идёт: дописанная реплика отбивается кодом `no-mode`, а в теме не появляется ничего.
// Вернули режим — следующая реплика уходит в тему как обычно.
func TestReplyStaysOutOfTopicWithoutMode(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	if off := rig.do(mod, rig.req(protocol.OpModeOff)); !off.OK {
		t.Fatalf("режим не снят: %+v", off.Error)
	}

	before := len(rig.api.texts(on.TopicID))

	reply := rig.req(protocol.OpAgentMessage)
	reply.Text = "реплика у терминала"

	resp := rig.do(mod, reply)
	if resp.OK || resp.Error == nil || resp.Error.Code != protocol.CodeNoMode {
		t.Fatalf("реплика при снятом режиме не отбита кодом no-mode: %+v", resp)
	}

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("реплика при снятом режиме ушла в тему: %v", texts[before:])
	}

	if back := rig.do(mod, rig.req(protocol.OpModeOn)); !back.OK {
		t.Fatalf("режим не вернулся: %+v", back.Error)
	}

	reply.Text = "реплика вне терминала"

	resp = rig.do(mod, reply)
	if !resp.OK {
		t.Fatalf("реплика с режимом не ушла: %+v", resp.Error)
	}

	texts := rig.api.texts(on.TopicID)
	if texts[len(texts)-1] != "реплика вне терминала" {
		t.Fatalf("последнее сообщение темы не реплика: %q", texts[len(texts)-1])
	}
}

// TestQuestionAskFromTool: вопрос владельцу задаёт и чужой инструмент — ему этот вход открыт, а
// поток сообщений агента нет.
func TestQuestionAskFromTool(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.mod("s1", topicAlpha)

	tool := rig.tool("s1")

	ask := rig.req(protocol.OpQuestionAsk)
	ask.Session = "s1"
	ask.Question = "выбери"

	if resp := rig.do(tool, ask); !resp.OK {
		t.Fatalf("инструменту вопрос недоступен: %+v", resp.Error)
	}

	stream := rig.req(protocol.OpAgentMessage)
	stream.Session = "s1"
	stream.Text = "мой вывод под видом реплики агента"

	if resp := rig.do(tool, stream); resp.OK {
		t.Fatal("чужой инструмент слил вывод в поток сообщений агента")
	}
}

// TestStreamNeedsSessionConn: поток ведёт соединение сессии, и только оно.
func TestStreamNeedsSessionConn(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	first, _ := rig.mod("s1", topicAlpha)

	// Вытеснение: сессию забирает новое соединение мода.
	second, back := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !back.OK {
		t.Fatalf("новое соединение сессии отказано: %+v", back.Error)
	}

	stream := rig.req(protocol.OpAgentMessage)
	stream.Text = "реплика"

	if resp := rig.do(first, stream); resp.OK {
		t.Fatal("вытесненное соединение ведёт поток")
	}

	if resp := rig.do(second, stream); !resp.OK {
		t.Fatalf("соединение сессии поток не ведёт: %+v", resp.Error)
	}
}

// TestSessionNeedsMod: сессии без мода не бывает — приветствие с чужим видом звонящего
// отбивается названной причиной, а не поднимает полуживую сессию.
func TestSessionNeedsMod(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn := rig.core.NewConn()
	req := rig.req(protocol.OpHello)
	req.Role = protocol.RoleSession
	req.Kind = "hook"
	req.Session = "s1"

	resp := rig.do(conn, req)
	if resp.OK || resp.Error.Code != protocol.CodeBadRequest {
		t.Fatalf("сессия без мода принята: %+v", resp)
	}

	if !strings.Contains(resp.Error.Text, "негодный вид звонящего") {
		t.Fatalf("причина отказа не названа: %q", resp.Error.Text)
	}
}

// TestSayKinds: видов сообщения два — обычное и извещение; вывода исполненной команды среди них
// нет, командная кухня в теме не видна.
func TestSayKinds(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, _ := rig.mod("s1", topicAlpha)

	say := rig.req(protocol.OpSay)
	say.Text = "слово"
	say.Kind = string(protocol.SayNotice)

	if resp := rig.do(mod, say); !resp.OK {
		t.Fatalf("извещение не отправлено: %+v", resp.Error)
	}

	say.Kind = "command-result"

	if resp := rig.do(mod, say); resp.OK {
		t.Fatal("вывод команды принят видом сообщения")
	}
}

// TestLateTurnEndDoesNotKillNextTurn: конец хода гасит «печатает» только своего хода. Извещения о
// границах ответа не ждут и уходят у средства сессии без ожидания, поэтому конец прошлого хода
// может прийти после начала следующего — и погасил бы «печатает» на весь следующий ход. Чужой
// конец проходит мимо и остаётся строкой в журнале.
func TestLateTurnEndDoesNotKillNextTurn(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, _ := rig.mod("s1", topicAlpha)

	rig.do(mod, turnBound(rig, protocol.OpTurnStart, "t-1"))
	rig.do(mod, turnBound(rig, protocol.OpTurnStart, "t-2"))

	// Конец первого хода обогнал начало второго: гасить ему нечего.
	late := rig.do(mod, turnBound(rig, protocol.OpTurnEnd, "t-1"))
	if !late.OK {
		t.Fatalf("конец чужого хода отказал: %+v", late.Error)
	}

	if count := journalCount(t, rig, "turn-end-foreign"); count != 1 {
		t.Fatalf("чужой конец хода не назван в журнале: %d", count)
	}

	// Конец второго хода — свой: он гасит и следов не оставляет.
	rig.do(mod, turnBound(rig, protocol.OpTurnEnd, "t-2"))

	if count := journalCount(t, rig, "turn-end-foreign"); count != 1 {
		t.Fatalf("свой конец хода назвался чужим: %d", count)
	}
}

// TestTurnStartAfterOwnEndIsIgnored: начало, опоздавшее за собственным концом, хода не начинает —
// гасить такое «печатает» было бы уже некому. Пропуск не бесследный: его видно журналом.
func TestTurnStartAfterOwnEndIsIgnored(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, _ := rig.mod("s1", topicAlpha)

	rig.do(mod, turnBound(rig, protocol.OpTurnEnd, "t-1"))
	rig.do(mod, turnBound(rig, protocol.OpTurnStart, "t-1"))

	// Срок щедрый: действие чата держится первым же вызовом, и если ход начался, вызов будет.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && rig.api.chatActions() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	if rig.api.chatActions() != 0 {
		t.Fatalf("начало за собственным концом завело «печатает»: %d", rig.api.chatActions())
	}

	if count := journalCount(t, rig, "turn-start-stale"); count != 1 {
		t.Fatalf("пропуск начала хода не назван в журнале: %d", count)
	}
}

// TestReconnectTakesSameTurnNameAgain: имя хода живёт у средства сессии, а не у службы. Подкоманда
// вышла и поднялась заново — счёт ходов у неё начинается с нуля, и первый ход после восстановления
// носит то же имя, что последний до обрыва. Память службы, пережившая обрыв, съела бы такой ход
// молча: «печатает» не встало бы на весь ход.
func TestReconnectTakesSameTurnNameAgain(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, _ := rig.mod("s1", topicAlpha)

	rig.do(mod, turnBound(rig, protocol.OpTurnStart, "1"))
	rig.do(mod, turnBound(rig, protocol.OpTurnEnd, "1"))
	waitChatActions(t, rig, 1)

	rig.closeConn(mod)

	back, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("возврат соединения отказал: %+v", resp.Error)
	}

	rig.do(back, turnBound(rig, protocol.OpTurnStart, "1"))
	waitChatActions(t, rig, 2)

	if count := journalCount(t, rig, "turn-start-stale"); count != 0 {
		t.Fatalf("первый ход после возврата пропущен: %d", count)
	}
}

// TestDisplacedConnForgetsTurnName: то же и при перехвате — мод перезапустился, не закрыв прежнее
// соединение, и новое приветствие вытесняет его. Ход прежнего соединения кончился вместе с ним.
func TestDisplacedConnForgetsTurnName(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, _ := rig.mod("s1", topicAlpha)

	rig.do(mod, turnBound(rig, protocol.OpTurnStart, "1"))
	rig.do(mod, turnBound(rig, protocol.OpTurnEnd, "1"))
	waitChatActions(t, rig, 1)

	fresh, resp := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		t.Fatalf("приветствие нового соединения отказало: %+v", resp.Error)
	}

	rig.do(fresh, turnBound(rig, protocol.OpTurnStart, "1"))
	waitChatActions(t, rig, 2)

	if count := journalCount(t, rig, "turn-start-stale"); count != 0 {
		t.Fatalf("первый ход вытеснившего соединения пропущен: %d", count)
	}
}

// TestContextReportLeavesTopicAloneWithoutMode: строка состояния живёт, пока владелец не у
// компьютера. Со снятым режимом процент тему не трогает вовсе — ни правкой, ни новым сообщением.
func TestContextReportLeavesTopicAloneWithoutMode(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)
	pinned := pinnedID(t, rig, on.TopicID)

	rig.do(mod, rig.req(protocol.OpModeOff))

	report := rig.req(protocol.OpContextReport)
	report.Percent = shownPercent

	resp := rig.do(mod, report)
	if !resp.OK {
		t.Fatalf("процент со снятым режимом отказал: %+v", resp.Error)
	}

	if texts := rig.api.texts(on.TopicID); len(texts) != 1 {
		t.Fatalf("процент со снятым режимом отправил в тему сообщение: %v", texts)
	}

	// Снятие режима правит строку само, одним именем темы; процент после него в неё не уезжает.
	if edit := editOf(t, rig, pinned); edit != topicAlpha {
		t.Fatalf("процент правил строку со снятым режимом: %q", edit)
	}
}

// turnBound собирает извещение о границе хода с именем хода.
func turnBound(rig *rig, op protocol.Op, turn string) protocol.Request {
	req := rig.req(op)
	req.Turn = turn

	return req
}

// waitChatActions ждёт, пока действий чата станет не меньше названного: «печатает» держится
// первым же вызовом, поэтому живой ход виден по их числу.
func waitChatActions(t *testing.T, rig *rig, want int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && rig.api.chatActions() < want {
		time.Sleep(5 * time.Millisecond)
	}

	if got := rig.api.chatActions(); got < want {
		t.Fatalf("действий чата %d, а ждали %d: ход «печатает» не держит", got, want)
	}
}

// journalCount считает, сколько раз журнал назвал событие.
func journalCount(t *testing.T, rig *rig, event string) int {
	t.Helper()

	count := 0

	for _, entry := range rig.journalEvents() {
		if entry.Event == event {
			count++
		}
	}

	return count
}
