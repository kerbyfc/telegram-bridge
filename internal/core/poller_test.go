package core_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/core"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// TestPollSavesOffsetPerUpdate: смещение двигается за каждое разобранное обновление, а не за всю
// пачку: пачка, упавшая на середине, была бы разобрана заново целиком, и уже отданные слова
// владельца уехали бы агенту вторыми записями.
func TestPollSavesOffsetPerUpdate(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicAlpha)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	go rig.core.Poll(ctx)

	for num := range 3 {
		rig.api.updates <- botapi.Update{
			ID: int64(70 + num),
			Message: &botapi.Message{
				ID: 9100 + num, TopicID: on.TopicID, Text: "слово",
				From:      &botapi.User{ID: ownerID, Name: "Александр", IsBot: false, IsSelf: false},
				At:        time.Now(),
				ReplyTo:   nil,
				TopicEdit: nil,
			},
		}
	}

	offsets := filepath.Join(rig.root, "updates.json")
	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) && readOffset(t, offsets) != 73 {
		time.Sleep(5 * time.Millisecond)
	}

	stop()

	if got := len(rig.inboxFiles("s1")); got != 3 {
		t.Fatalf("разобраны не все обновления пачки: %d", got)
	}

	if got := readOffset(t, offsets); got != 73 {
		t.Fatalf("смещение не за последнее разобранное обновление: %d", got)
	}
}

// TestPollStartsFromSavedOffset: служба начинает первый опрос со смещения, лежащего в её
// состоянии, — в том числе перенесённого со старого моста. Так Telegram подтверждает обновление,
// которое прежний опрашивающий разобрал, но подтвердить не успел, и второй раз его не отдаёт.
func TestPollStartsFromSavedOffset(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	saved := store.Updates{Offset: 709112834}

	err := saved.Save(rig.store)
	if err != nil {
		t.Fatalf("смещение не положено: %v", err)
	}

	rig.reopen()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	go rig.core.Poll(ctx)

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		first, polled := rig.api.firstPoll()
		if polled {
			if first != saved.Offset {
				t.Fatalf("первый опрос со смещения %d, а в состоянии %d", first, saved.Offset)
			}

			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("опроса не было")
}

// TestPollFloodWaitsNamedPauseAndSurvives: отказ опроса службу не роняет и в тишину не уводит.
// Флуд-лимит — пауза ровно того срока, который назвал Telegram, а не повтор впритык и не пауза
// повтора; за чередой сетевых отказов держится последняя пауза повтора, так что мёртвый канал
// не крутит опрос и не топит журнал. Каждый отказ — одна строка журнала со своим
// кодом, исход опроса называет самопроверка, а первый удачный опрос его снимает.
//
//nolint:cyclop,funlen // один путь поллера от флуд-лимита до удачного опроса: каждый шаг — своя проверка
func TestPollFloodWaitsNamedPauseAndSurvives(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	const (
		flood = 300 * time.Millisecond
		last  = 40 * time.Millisecond
	)

	rig.cfg.RetryPauses = []time.Duration{time.Millisecond, last}

	network := botapi.Refuse(protocol.CodeNetwork, "getUpdates: сеть до Bot API не отвечает")
	rig.api.fail(methodGetUpdates,
		&botapi.Error{
			Code: protocol.CodeRateLimited, Text: "getUpdates: Too Many Requests: retry after 1 (429)",
			RetryAfter: flood, Cause: nil,
		},
		network, network, network)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	go rig.core.Poll(ctx)

	status := pollStatus(t, rig, func(poll protocol.PollStatus) bool { return poll.At != "" })
	if status.OK || status.Code != protocol.CodeRateLimited {
		t.Fatalf("самопроверка не назвала флуд-лимит опроса: %+v", status)
	}

	status = pollStatus(t, rig, func(poll protocol.PollStatus) bool { return poll.OK })

	stop()

	if status.Code != "" || status.Text != "" {
		t.Fatalf("удачный опрос не снял прежний отказ: %+v", status)
	}

	polls := rig.api.pollTimes()
	if len(polls) < 5 {
		t.Fatalf("опросов %d, а после четырёх отказов был удачный", len(polls))
	}

	if gap := polls[1].Sub(polls[0]); gap < flood {
		t.Fatalf("после флуд-лимита опрос повторён через %s, а Telegram назвал %s", gap, flood)
	}

	for at := 2; at <= 4; at++ {
		if gap := polls[at].Sub(polls[at-1]); gap < last {
			t.Fatalf("опрос %d повторён через %s, а пауза повтора %s", at+1, gap, last)
		}
	}

	codes := make([]string, 0, 4)

	for _, entry := range rig.journalEvents() {
		if entry.Event == "poll-failed" {
			codes = append(codes, entry.Code)
		}
	}

	want := []string{string(protocol.CodeRateLimited), "network", "network", "network"}
	if !slices.Equal(codes, want) {
		t.Fatalf("строки журнала об отказах опроса %v, а ждали %v", codes, want)
	}
}

// TestSlicedQuestionAnswerComesAsWord: вопрос, нарезанный на несколько сообщений, закрывать нечем —
// ожидания хода нет, и в теме он обычный текст. Реакция владельца на любой его кусок входом не
// является: клиент Bot API отдаёт такое обновление пустым, разбор его пропускает, смещение опроса
// уходит дальше, а в теме, в ящике и в отметках ничего не появляется. Ответ владельца приходит
// сессии обычной доставкой слова, и тему вопрос не переименовывает.
//
//nolint:cyclop,funlen // один путь нарезанного вопроса от отправки до ответа: каждый шаг — своя проверка
func TestSlicedQuestionAnswerComesAsWord(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	ask := rig.req(protocol.OpQuestionAsk)
	ask.Question = strings.Repeat("ю", 4500)

	asked := rig.do(mod, ask)
	if !asked.OK || len(asked.MessageIDs) != 2 {
		t.Fatalf("вопрос не нарезан на два сообщения: %+v", asked)
	}

	before := len(rig.api.texts(on.TopicID))

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	go rig.core.Poll(ctx)

	rig.api.updates <- botapi.Update{ID: 80, Message: nil}

	rig.api.updates <- botapi.Update{
		ID: 81,
		Message: &botapi.Message{
			ID: 9200, TopicID: on.TopicID, Text: "да, делай",
			From: &botapi.User{ID: ownerID, Name: "Александр", IsBot: false, IsSelf: false},
			At:   time.Now(), ReplyTo: nil, TopicEdit: nil,
		},
	}

	got := rig.waitDelivery(mod)
	if !got.OK || got.Kind != protocol.DeliveryOwnerMessage || got.Text != "да, делай" {
		t.Fatalf("ответ на нарезанный вопрос не пришёл словом владельца: %+v", got)
	}

	offsets := filepath.Join(rig.root, "updates.json")
	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) && readOffset(t, offsets) != 82 {
		time.Sleep(5 * time.Millisecond)
	}

	stop()

	if offset := readOffset(t, offsets); offset != 82 {
		t.Fatalf("смещение опроса %d: пустое обновление его задержало", offset)
	}

	if files := rig.inboxFiles("s1"); len(files) != 1 {
		t.Fatalf("в ящике не одно слово ответа: %v", files)
	}

	if texts := rig.api.texts(on.TopicID); len(texts) != before {
		t.Fatalf("служба заговорила в теме о реакции или ответе: %v", texts[before:])
	}

	for _, id := range asked.MessageIDs {
		if mark := rig.api.mark(id); mark != "" {
			t.Fatalf("кусок вопроса %d получил отметку: %q", id, mark)
		}
	}

	if names := rig.api.renamedNames(); len(names) != 0 {
		t.Fatalf("вопрос переименовал тему: %v", names)
	}
}

// TestBrokenOffsetRefusesStart: смещение опроса пишется атомарной заменой, и нечитаемым его
// оставить может только чужая рука или порча диска. Такое смещение — громкий отказ старта с
// названным файлом, а не опрос с нуля: служба не делает вид, что состояние цело, и не молчит
// поллером на нечитаемом числе.
func TestBrokenOffsetRefusesStart(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"не JSON":     "смещение",
		"обрыв":       `{"offset": 12`,
		"не число":    `{"offset": "двенадцать"}`,
		"пустой файл": "",
	} {
		rig := newRig(t)
		path := filepath.Join(rig.root, "updates.json")

		err := os.WriteFile(path, []byte(body), store.FilePerm)
		if err != nil {
			t.Fatalf("%s: смещение не записано: %v", name, err)
		}

		_, err = core.New(rig.cfg, rig.api, rig.store, rig.jrnl)
		if err == nil {
			t.Fatalf("%s: служба поднялась на нечитаемом смещении", name)
		}

		if !strings.Contains(err.Error(), "смещение опроса не прочитано") || !strings.Contains(err.Error(), path) {
			t.Fatalf("%s: отказ старта не назвал смещение и его файл: %v", name, err)
		}
	}
}

// TestInterruptedWriteKeepsPreviousState: обрыв атомарной замены посреди записи — падение службы,
// отказ питания — оставляет рядом с целью временный файл, а цель прежней. Поднявшаяся служба
// временных файлов не читает и встаёт на прежнем состоянии: смещение опроса, привязка сессии,
// режим и неподтверждённое слово владельца целы.
func TestInterruptedWriteKeepsPreviousState(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)
	rig.ownerMessage(on.TopicID, "слово до обрыва")
	rig.closeConn(mod)

	err := (&store.Updates{Offset: 77}).Save(rig.store)
	if err != nil {
		t.Fatalf("смещение не записано: %v", err)
	}

	for _, dir := range []string{rig.root, filepath.Join(rig.root, "sessions"), filepath.Join(rig.root, "inbox", "s1")} {
		err = os.WriteFile(filepath.Join(dir, ".tmp-123456"), []byte(`{"offset": 1`), store.FilePerm)
		if err != nil {
			t.Fatalf("след обрыва в %s не положен: %v", dir, err)
		}
	}

	rig.reopen()

	back, hello := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !hello.OK || hello.Mode != protocol.ModeOn || hello.TopicID != on.TopicID {
		t.Fatalf("сессия не застала прежний режим и тему: %+v", hello)
	}

	if got := rig.waitDelivery(back); got.Text != "слово до обрыва" {
		t.Fatalf("неподтверждённое слово не пережило обрыв: %+v", got)
	}

	if offset := readOffset(t, filepath.Join(rig.root, "updates.json")); offset != 77 {
		t.Fatalf("смещение опроса после обрыва %d, а записано 77", offset)
	}
}

// pollStatus спрашивает исход опроса у самопроверки, пока он не станет таким, как ждёт тест.
func pollStatus(t *testing.T, rig *rig, ready func(protocol.PollStatus) bool) protocol.PollStatus {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		resp := rig.do(rig.tool(""), rig.req(protocol.OpServiceStatus))
		if resp.Service != nil && ready(resp.Service.Poll) {
			return resp.Service.Poll
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("исход опроса не дошёл до самопроверки")

	return protocol.PollStatus{At: "", OK: false, Code: "", Text: ""}
}

// readOffset читает смещение опроса с диска; файла ещё нет — нуль.
func readOffset(t *testing.T, path string) int64 {
	t.Helper()

	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}

	if err != nil {
		t.Fatalf("смещение не прочитано: %v", err)
	}

	var upd store.Updates

	err = json.Unmarshal(body, &upd)
	if err != nil {
		t.Fatalf("смещение не разобрано: %v", err)
	}

	return upd.Offset
}
