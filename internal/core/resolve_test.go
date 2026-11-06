package core_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/core"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/resolve/resolvetest"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

func TestMain(m *testing.M) {
	resolvetest.Serve()
	os.Exit(m.Run())
}

// askWhere и askLink — место и ссылка записи разворота в тестах.
const (
	askWhere = `(?m)^needs? input:(.*)$`
	askLink  = `x://[^\s)]+`
)

// withResolver добавляет стенду запись разворота с поддельной командой.
func (r *rig) withResolver(mode string, timeout time.Duration) {
	r.t.Helper()

	res, err := config.NewLinkResolver(askWhere, askLink, resolvetest.Command(mode, "{link}"), timeout)
	if err != nil {
		r.t.Fatalf("запись не собрана: %v", err)
	}

	r.cfg.LinkResolvers = append(r.cfg.LinkResolvers, res)
}

// reply отправляет реплику агента и требует успеха.
func (r *rig) reply(conn *core.Conn, text string) protocol.Response {
	r.t.Helper()

	req := r.req(protocol.OpAgentMessage)
	req.Text = text

	resp := r.do(conn, req)
	if !resp.OK {
		r.t.Fatalf("реплика отказала: %+v", resp.Error)
	}

	return resp
}

// waitTexts ждёт, пока в теме ляжет столько сообщений, и отдаёт их без закреплённой строки.
func (r *rig) waitTexts(topicID int64, count int) []string {
	r.t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for {
		texts := r.api.texts(topicID)[1:]
		if len(texts) >= count {
			return texts
		}

		if time.Now().After(deadline) {
			r.t.Fatalf("в теме не легло %d сообщений: %v", count, texts)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestБезЗаписейРазворотаРеплика одна: настройки нет — реплика со ссылкой уходит одним
// сообщением, как была.
func TestБезЗаписейРазворотаРепликаОдна(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	rig.reply(conn, "needs input: [Вопрос](x://один)")
	rig.reply(conn, "дальше")

	texts := rig.waitTexts(on.TopicID, 2)
	if !slices.Equal(texts, []string{"needs input: [Вопрос](x://один)", "дальше"}) {
		t.Fatalf("тема не та: %v", texts)
	}
}

// TestОднаСсылкаРазворачиваетсяПослеРеплики: вывод команды встаёт отдельным сообщением следом
// за репликой, с переводами строк `\n`.
func TestОднаСсылкаРазворачиваетсяПослеРеплики(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.withResolver(resolvetest.ModePrint, 10*time.Second)
	conn, on := rig.mod("s1", topicAlpha)

	resp := rig.reply(conn, "готово\nneeds input: [Вопрос](x://один)")
	if len(resp.MessageIDs) != 1 {
		t.Fatalf("ответ на реплику назвал не её одну: %v", resp.MessageIDs)
	}

	texts := rig.waitTexts(on.TopicID, 2)
	if !slices.Equal(texts, []string{"готово\nneeds input: [Вопрос](x://один)", "развёрнуто x://один\nвторая строка"}) {
		t.Fatalf("тема не та: %q", texts)
	}
}

// TestНесколькоСсылокПоПорядку: каждая ссылка места даёт своё сообщение по порядку текста, а
// следующая реплика встаёт после вывода прошлой.
func TestНесколькоСсылокПоПорядку(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.withResolver(resolvetest.ModePrint, 10*time.Second)
	conn, on := rig.mod("s1", topicAlpha)

	first := "needs input: [а](x://один), [б](x://два)\nneeds input: [в](x://три)"
	rig.reply(conn, first)
	rig.reply(conn, "следующая")

	texts := rig.waitTexts(on.TopicID, 5)
	want := []string{
		first,
		"развёрнуто x://один\nвторая строка",
		"развёрнуто x://два\nвторая строка",
		"развёрнуто x://три\nвторая строка",
		"следующая",
	}

	if !slices.Equal(texts, want) {
		t.Fatalf("порядок темы не тот: %q", texts)
	}
}

// TestСсылкаВнеМестаНеРазворачивается: ссылка той же формы вне места команды не зовёт.
func TestСсылкаВнеМестаНеРазворачивается(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.withResolver(resolvetest.ModePrint, 10*time.Second)
	conn, on := rig.mod("s1", topicAlpha)

	rig.reply(conn, "смотри x://один")
	rig.reply(conn, "needs input: вопрос в теме")
	rig.reply(conn, "конец")

	texts := rig.waitTexts(on.TopicID, 3)
	if !slices.Equal(texts, []string{"смотри x://один", "needs input: вопрос в теме", "конец"}) {
		t.Fatalf("тема не та: %q", texts)
	}
}

// TestУпавшаяКомандаДаётСтрокуОтказа: ненулевой код — реплика в теме, следом строка отказа с
// ссылкой и причиной, и строка журнала.
func TestУпавшаяКомандаДаётСтрокуОтказа(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.withResolver(resolvetest.ModeFail, 10*time.Second)
	conn, on := rig.mod("s1", topicAlpha)

	rig.reply(conn, "needs input: [а](x://один)")

	texts := rig.waitTexts(on.TopicID, 2)
	if texts[1] != core.UnresolvedPrefix+"x://один: код выхода 3: нет такой заметки" {
		t.Fatalf("строка отказа не та: %q", texts)
	}

	waitJournal(t, rig, "link-unresolved")
}

// TestСрокКомандыДаётСтрокуОтказа: команда не уложилась в срок — строка отказа называет срок, а
// ответ на реплику его не ждал.
func TestСрокКомандыДаётСтрокуОтказа(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.withResolver(resolvetest.ModeSleep, 300*time.Millisecond)
	conn, on := rig.mod("s1", topicAlpha)

	started := time.Now()

	rig.reply(conn, "needs input: [а](x://один)")

	if spent := time.Since(started); spent >= 300*time.Millisecond {
		t.Fatalf("ответ на реплику ждал команду: %s", spent)
	}

	texts := rig.waitTexts(on.TopicID, 2)
	if texts[1] != core.UnresolvedPrefix+"x://один: срок 300ms вышел" {
		t.Fatalf("строка отказа не та: %q", texts)
	}
}

// waitJournal ждёт строку журнала с событием.
func waitJournal(t *testing.T, rig *rig, event string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		if slices.ContainsFunc(rig.journalEvents(), func(e store.Entry) bool {
			return e.Event == event && strings.Contains(e.Text, "x://")
		}) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("в журнале нет %s", event)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestВыводМедленнойКомандыДоСледующейРеплики: команда первой реплики думает, вторая реплика
// приходит раньше её вывода — в теме всё равно реплика, её вывод, следующая реплика.
func TestВыводМедленнойКомандыДоСледующейРеплики(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.withResolver(resolvetest.ModeSlow, 10*time.Second)
	conn, on := rig.mod("s1", topicAlpha)

	rig.reply(conn, "needs input: [а](x://один)")
	rig.reply(conn, "следующая")

	texts := rig.waitTexts(on.TopicID, 3)
	want := []string{"needs input: [а](x://один)", "развёрнуто x://один\nвторая строка", "следующая"}

	if !slices.Equal(texts, want) {
		t.Fatalf("порядок темы не тот: %q", texts)
	}
}

// TestSayИВопросПослеВыводаРазворота: сказанное и вопрос с вариантами встают после вывода
// разворота прошлой реплики, а не между репликой и выводом.
func TestSayИВопросПослеВыводаРазворота(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.withResolver(resolvetest.ModeSlow, 10*time.Second)
	conn, on := rig.mod("s1", topicAlpha)

	rig.reply(conn, "needs input: [а](x://один)")

	say := rig.req(protocol.OpSay)
	say.Kind = string(protocol.SayMessage)
	say.Text = "сказано"

	if resp := rig.do(conn, say); !resp.OK {
		t.Fatalf("say отказал: %+v", resp.Error)
	}

	rig.reply(conn, "needs input: [б](x://два)")

	ask := rig.req(protocol.OpQuestionAsk)
	ask.Question = "вопрос"

	if resp := rig.do(conn, ask); !resp.OK {
		t.Fatalf("question.ask отказал: %+v", resp.Error)
	}

	texts := rig.waitTexts(on.TopicID, 6)
	want := []string{
		"needs input: [а](x://один)", "развёрнуто x://один\nвторая строка", "сказано",
		"needs input: [б](x://два)", "развёрнуто x://два\nвторая строка", "вопрос",
	}

	if !slices.Equal(texts, want) {
		t.Fatalf("порядок темы не тот: %q", texts)
	}
}

// TestОтказОтправкиРепликиСнимаетКоманду: реплика в тему не легла — команда снята, и ни вывода,
// ни строки отказа в теме нет.
func TestОтказОтправкиРепликиСнимаетКоманду(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.withResolver(resolvetest.ModeSleep, time.Minute)
	conn, on := rig.mod("s1", topicAlpha)

	rig.api.fail(methodSendMessage, botapi.Refuse(protocol.CodeTelegramRefused, "отказ"))

	req := rig.req(protocol.OpAgentMessage)
	req.Text = "needs input: [а](x://один)"

	if resp := rig.do(conn, req); resp.OK {
		t.Fatalf("реплика при отказе платформы прошла: %+v", resp)
	}

	idle := make(chan struct{})

	go func() {
		core.WaitResolving(rig.core)
		close(idle)
	}()

	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("команда не снята после отказа отправки реплики")
	}

	if texts := rig.api.texts(on.TopicID)[1:]; len(texts) != 0 {
		t.Fatalf("в теме что-то легло: %q", texts)
	}
}

// TestОтказОтправкиВыводаВЖурнале: вывод не отправился — строка журнала `link-send-failed`.
func TestОтказОтправкиВыводаВЖурнале(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.withResolver(resolvetest.ModeSlow, 10*time.Second)
	conn, _ := rig.mod("s1", topicAlpha)

	rig.reply(conn, "needs input: [а](x://один)")
	rig.api.fail(methodSendMessage, botapi.Refuse(protocol.CodeTelegramRefused, "отказ"))

	waitJournal(t, rig, "link-send-failed")
}

// TestОстановкаСлужбыБросаетРазворот: остановка снимает команды, вывод в тему не идёт, а каждая
// брошенная ссылка оставляет строку журнала `link-dropped`. Новая реплика после остановки команд
// не запускает.
func TestОстановкаСлужбыБросаетРазворот(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	rig.withResolver(resolvetest.ModeSleep, time.Minute)
	conn, on := rig.mod("s1", topicAlpha)

	rig.reply(conn, "needs input: [а](x://один)")

	started := time.Now()

	rig.core.Close()

	if spent := time.Since(started); spent > 5*time.Second {
		t.Fatalf("остановка ждала команду: %s", spent)
	}

	waitJournal(t, rig, "link-dropped")

	if texts := rig.api.texts(on.TopicID)[1:]; len(texts) != 1 {
		t.Fatalf("после остановки в тему легло лишнее: %q", texts)
	}

	rig.reply(conn, "needs input: [б](x://два)")
	core.WaitResolving(rig.core)
}
