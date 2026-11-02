package botapi_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
)

// updatesBody — ответ опроса: слово владельца в теме, своё же сообщение, вернувшееся тем же
// опросом, сообщение чужого чата и сообщение без автора.
var updatesBody = fmt.Sprintf(`{"ok":true,"result":[
  {"update_id":70,"message":{"message_id":1,"message_thread_id":501,"date":1700000000,
    "chat":{"id":%d},"from":{"id":%d,"is_bot":false,"first_name":"Александр"},"text":"слово"}},
  {"update_id":71,"message":{"message_id":2,"message_thread_id":501,"date":1700000001,
    "chat":{"id":%d},"from":{"id":%d,"is_bot":true,"username":"dm_bot"},"text":"ответ"}},
  {"update_id":72,"message":{"message_id":3,"date":1700000002,
    "chat":{"id":-100999},"from":{"id":5,"is_bot":false,"first_name":"Чужой"},"text":"чужое"}},
  {"update_id":73,"message":{"message_id":4,"message_thread_id":501,"date":1700000003,
    "chat":{"id":%d},"text":"от имени группы"}}
]}`, groupID, ownerID, groupID, selfID, groupID)

// TestGetUpdatesMapsOwnGroupOnly: опрос отдаёт ядру обновления своей группы; чужой чат доходит
// пустым, а не выброшенным целиком — иначе смещение встало бы на нём и чужое возвращалось бы
// вечно. Своё сообщение помечено своим авторством: кто такой «свой», знает клиент — у него токен.
func TestGetUpdatesMapsOwnGroupOnly(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(updatesBody))

	updates, err := harness.client(t).GetUpdates(context.Background(), 70, 25*time.Second)
	if err != nil {
		t.Fatalf("опрос отказал: %v", err)
	}

	if len(updates) != 4 {
		t.Fatalf("обновлений %d, а в ответе четыре", len(updates))
	}

	ownerWord(t, updates[0])

	if !updates[1].FromSelf() {
		t.Fatal("своё сообщение не признано своим")
	}

	if updates[2].ID != 72 || updates[2].Message != nil {
		t.Fatalf("сообщение чужого чата: %+v", updates[2])
	}

	if updates[3].Message == nil || updates[3].Message.From != nil {
		t.Fatalf("сообщение без автора: %+v", updates[3].Message)
	}
}

// TestGetUpdatesNamesOffsetAndKinds: опрос идёт со смещения, со своим сроком и только за теми
// видами обновлений, которые службе что-то говорят.
func TestGetUpdatesNamesOffsetAndKinds(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(`{"ok":true,"result":[]}`))

	_, err := harness.client(t).GetUpdates(context.Background(), 70, 25*time.Second)
	if err != nil {
		t.Fatalf("опрос отказал: %v", err)
	}

	calls := harness.recorded()
	if calls[0].method != "getUpdates" {
		t.Fatalf("метод вызова: %s", calls[0].method)
	}

	if number(t, calls[0].body, "offset") != 70 || number(t, calls[0].body, "timeout") != 25 {
		t.Fatalf("кадр опроса: %v", calls[0].body)
	}

	// Размер пачки назван: от него считается предел читаемого ответа, и «сколько дадут»
	// делало бы этот предел недоказуемым.
	if number(t, calls[0].body, "limit") != 100 {
		t.Fatalf("размер пачки в кадре: %v", calls[0].body)
	}

	kinds, ok := field(t, calls[0].body, "allowed_updates").([]any)
	if !ok || len(kinds) != 1 || kinds[0] != "message" {
		t.Fatalf("виды обновлений в кадре: %v", calls[0].body)
	}
}

// topicEditsBody — служебные сообщения о правке темы (`forum_topic_edited`): правка имени без
// значка, снятый значок, поставленный значок — и обычное слово, у которого правки нет.
var topicEditsBody = fmt.Sprintf(`{"ok":true,"result":[
  {"update_id":80,"message":{"message_id":11,"message_thread_id":501,"date":1700000010,
    "chat":{"id":%d},"from":{"id":%d,"is_bot":false,"first_name":"Александр"},
    "forum_topic_edited":{"name":"новое имя"}}},
  {"update_id":81,"message":{"message_id":12,"message_thread_id":501,"date":1700000011,
    "chat":{"id":%d},"from":{"id":%d,"is_bot":false,"first_name":"Александр"},
    "forum_topic_edited":{"icon_custom_emoji_id":""}}},
  {"update_id":82,"message":{"message_id":13,"message_thread_id":501,"date":1700000012,
    "chat":{"id":%d},"from":{"id":%d,"is_bot":false,"first_name":"Александр"},
    "forum_topic_edited":{"icon_custom_emoji_id":"5312536423851630001"}}},
  {"update_id":83,"message":{"message_id":14,"message_thread_id":501,"date":1700000013,
    "chat":{"id":%d},"from":{"id":%d,"is_bot":false,"first_name":"Александр"},"text":"слово"}}
]}`, groupID, ownerID, groupID, ownerID, groupID, ownerID, groupID, ownerID)

// TestGetUpdatesReadsTopicEdits: правка темы доходит до ядра правкой, а не словом владельца. Поля
// правки необязательные: значка в кадре нет — правка его не меняла, пустая строка — значок снят.
func TestGetUpdatesReadsTopicEdits(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(topicEditsBody))

	updates, err := harness.client(t).GetUpdates(context.Background(), 80, 25*time.Second)
	if err != nil {
		t.Fatalf("опрос отказал: %v", err)
	}

	if len(updates) != 4 {
		t.Fatalf("обновлений %d, а в ответе четыре", len(updates))
	}

	want := []*botapi.TopicEdit{
		{IconChanged: false, Icon: ""},
		{IconChanged: true, Icon: ""},
		{IconChanged: true, Icon: "5312536423851630001"},
		nil,
	}

	for at, edit := range want {
		got := updates[at].Message.TopicEdit
		if (got == nil) != (edit == nil) || (got != nil && *got != *edit) {
			t.Fatalf("правка %d: %+v, а ждали %+v", at+1, got, edit)
		}
	}
}

// wordlessBody — сообщения владельца без текста: стикер, фото без подписи и служебное «тема
// закрыта».
var wordlessBody = fmt.Sprintf(`{"ok":true,"result":[
  {"update_id":90,"message":{"message_id":21,"message_thread_id":501,"date":1700000020,
    "chat":{"id":%d},"from":{"id":%d,"is_bot":false,"first_name":"Александр"},
    "sticker":{"file_id":"CAACAgIAAxkBAAE","file_unique_id":"AgADBQ","type":"regular",
      "width":512,"height":512,"is_animated":false,"is_video":false}}},
  {"update_id":91,"message":{"message_id":22,"message_thread_id":501,"date":1700000021,
    "chat":{"id":%d},"from":{"id":%d,"is_bot":false,"first_name":"Александр"},
    "photo":[{"file_id":"AgACAgIAAxkBAAE","file_unique_id":"AQADBQ","width":90,"height":90}]}},
  {"update_id":92,"message":{"message_id":23,"message_thread_id":501,"date":1700000022,
    "chat":{"id":%d},"from":{"id":%d,"is_bot":false,"first_name":"Александр"},
    "forum_topic_closed":{}}}
]}`, groupID, ownerID, groupID, ownerID, groupID, ownerID)

// TestGetUpdatesGivesWordlessMessagesNoText: стикер, фото без подписи и служебное «тема закрыта»
// доходят до ядра сообщениями без текста и без правки темы — по этому ядро и не берёт их входом.
func TestGetUpdatesGivesWordlessMessagesNoText(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(wordlessBody))

	updates, err := harness.client(t).GetUpdates(context.Background(), 90, 25*time.Second)
	if err != nil {
		t.Fatalf("опрос отказал: %v", err)
	}

	if len(updates) != 3 {
		t.Fatalf("обновлений %d, а в ответе три", len(updates))
	}

	for _, upd := range updates {
		if upd.Message == nil || upd.Message.Text != "" || upd.Message.TopicEdit != nil || upd.Message.From == nil {
			t.Fatalf("сообщение без текста: %+v", upd.Message)
		}
	}
}

// reactionBody — пачка опроса с обновлениями, на которые служба не подписана: подписка
// `allowed_updates` не касается обновлений, накопленных до вызова с ней, и опрос может нести
// реакцию владельца на кусок нарезанного вопроса и правку сообщения. Между ними ответ владельца
// реплаем на тот же кусок.
var reactionBody = fmt.Sprintf(`{"ok":true,"result":[
  {"update_id":80,"message_reaction":{"chat":{"id":%d},"message_id":12,"date":1700000004,
    "user":{"id":%d,"is_bot":false,"first_name":"Александр"},
    "old_reaction":[],"new_reaction":[{"type":"emoji","emoji":"👍"}]}},
  {"update_id":81,"message":{"message_id":13,"message_thread_id":501,"date":1700000005,
    "chat":{"id":%d},"from":{"id":%d,"is_bot":false,"first_name":"Александр"},
    "reply_to_message":{"message_id":12,"message_thread_id":501,"date":1700000003,
      "chat":{"id":%d},"from":{"id":%d,"is_bot":true,"username":"dm_bot"},"text":"кусок"},
    "text":"да"}},
  {"update_id":82,"edited_message":{"message_id":13,"message_thread_id":501,"date":1700000005,
    "edit_date":1700000006,"chat":{"id":%d},"from":{"id":%d,"is_bot":false,"first_name":"Александр"},
    "text":"да, делай"}}
]}`, groupID, ownerID, groupID, ownerID, groupID, selfID, groupID, ownerID)

// TestNonMessageUpdatesComeEmpty: реакция и правка сообщения входом не являются — в теме видно
// только текст, и значки ставит сама служба отметками доставки. Такие обновления доходят до ядра
// пустыми, но со своим номером: смещение опроса уходит дальше них, а не встаёт навсегда. Ответ
// реплаем на кусок вопроса — обычное сообщение своей темы: реплай служба не читает.
//
//nolint:cyclop // одна пачка опроса разбирается целиком: каждое обновление — своя проверка
func TestNonMessageUpdatesComeEmpty(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(reactionBody))

	updates, err := harness.client(t).GetUpdates(context.Background(), 80, 25*time.Second)
	if err != nil {
		t.Fatalf("опрос отказал на реакции: %v", err)
	}

	if len(updates) != 3 {
		t.Fatalf("обновлений %d, а в ответе три", len(updates))
	}

	for _, at := range []int{0, 2} {
		if updates[at].ID != int64(80+at) || updates[at].Message != nil {
			t.Fatalf("обновление %d не пустое или без своего номера: %+v", 80+at, updates[at])
		}
	}

	answer := updates[1].Message
	if updates[1].ID != 81 || answer == nil || answer.Text != "да" || answer.TopicID != 501 {
		t.Fatalf("ответ реплаем на кусок вопроса: %+v", answer)
	}

	if answer.From == nil || answer.From.ID != ownerID || updates[1].FromSelf() {
		t.Fatalf("автор ответа реплаем: %+v", answer.From)
	}
}

// ownerWord сверяет слово владельца: текст, тема, автор и время его сообщения.
func ownerWord(t *testing.T, upd botapi.Update) {
	t.Helper()

	if upd.Message == nil || upd.Message.Text != "слово" || upd.Message.TopicID != 501 {
		t.Fatalf("слово владельца: %+v", upd.Message)
	}

	if upd.Message.From == nil || upd.Message.From.ID != ownerID || upd.FromSelf() {
		t.Fatalf("автор слова владельца: %+v", upd.Message.From)
	}

	if upd.Message.At.Unix() != 1700000000 {
		t.Fatalf("время сообщения: %v", upd.Message.At)
	}
}
