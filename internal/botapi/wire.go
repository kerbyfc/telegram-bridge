// Кадры Bot API: что уезжает в вызов и что приходит ответом. Имена полей заданы платформой,
// поэтому у файла снят `tagliatelle`.
//
//nolint:tagliatelle // имена полей заданы Bot API и пишутся как в нём
package botapi

import "encoding/json"

// envelope — общий конверт ответа Bot API: удача в `ok`, отказ в описании и коде, срок повтора в
// параметрах. Отдельного тела у отказа нет — он приходит тем же конвертом.
type envelope struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  *envelopeParams `json:"parameters"`
	Result      json.RawMessage `json:"result"`
}

// envelopeParams — параметры отказа; из них службе нужен только срок повтора флуд-лимита.
type envelopeParams struct {
	RetryAfter int `json:"retry_after"`
}

// emptyParams — вызов без параметров.
type emptyParams struct{}

// sendMessageParams — отправка текста в тему. Разметки нет: текст агента уходит как есть, иначе
// случайная звёздочка в его реплике отбивалась бы отказом разбора.
//
// Предпросмотр ссылок выключен решением владельца: реплики агента сплошь со ссылками, и карточка
// на каждую растянула бы тему, которую этот этап как раз вычищал от лишнего. Откат — одно поле.
type sendMessageParams struct {
	ChatID   int64         `json:"chat_id"`
	ThreadID int64         `json:"message_thread_id,omitempty"`
	Text     string        `json:"text"`
	Preview  previewOption `json:"link_preview_options"`
}

// editTextParams — правка текста отправленного сообщения на месте. Правит служба одно сообщение —
// закреплённую строку темы, — и предпросмотр выключен и здесь: поле идёт с каждой правкой, иначе
// правка вернула бы сообщению карточку по умолчанию.
type editTextParams struct {
	ChatID    int64         `json:"chat_id"`
	MessageID int           `json:"message_id"`
	Text      string        `json:"text"`
	Preview   previewOption `json:"link_preview_options"`
}

// previewOption — настройка предпросмотра ссылок у сообщения.
type previewOption struct {
	Disabled bool `json:"is_disabled"`
}

// topicNameParams — заведение темы с именем.
type topicNameParams struct {
	ChatID int64  `json:"chat_id"`
	Name   string `json:"name"`
}

// topicRenameParams — переименование темы. Значок при этом не трогается: поля значка в кадре нет,
// и Telegram оставляет прежний.
type topicRenameParams struct {
	ChatID   int64  `json:"chat_id"`
	ThreadID int64  `json:"message_thread_id"`
	Name     string `json:"name"`
}

// topicIconParams — переключение значка темы. Поле значка пишется всегда, в том числе пустым:
// пустое значение снимает значок, а отсутствие поля оставило бы прежний.
type topicIconParams struct {
	ChatID   int64  `json:"chat_id"`
	ThreadID int64  `json:"message_thread_id"`
	Icon     string `json:"icon_custom_emoji_id"`
}

// reactionParams — отметка доставки на сообщении владельца. Реакция у бота одна, поэтому список
// всегда из одного значка: новая отметка вытесняет прежнюю.
type reactionParams struct {
	ChatID    int64      `json:"chat_id"`
	MessageID int        `json:"message_id"`
	Reaction  []reaction `json:"reaction"`
}

// reaction — один значок реакции.
type reaction struct {
	Type  string `json:"type"`
	Emoji string `json:"emoji"`
}

// pinParams — закреп сообщения в теме. Без извещения: закреп — шапка темы, а не новость.
type pinParams struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int   `json:"message_id"`
	Silent    bool  `json:"disable_notification"`
}

// chatActionParams — действие чата в теме: «печатает», которое гаснет само.
type chatActionParams struct {
	ChatID   int64  `json:"chat_id"`
	ThreadID int64  `json:"message_thread_id,omitempty"`
	Action   string `json:"action"`
}

// getUpdatesParams — долгий опрос со смещения. Виды обновлений названы: смена реакции и правка
// сообщения службе не говорят ничего, а в теме видно только текст. Размер пачки назван тоже: от
// него считается предел читаемого ответа, а молчаливое «сколько дадут» делало бы этот предел
// недоказуемым.
type getUpdatesParams struct {
	Offset  int64    `json:"offset"`
	Timeout int      `json:"timeout"`
	Limit   int      `json:"limit"`
	Allowed []string `json:"allowed_updates"`
}

// messageResult — ответ отправки: id сообщения.
type messageResult struct {
	MessageID int `json:"message_id"`
}

// topicResult — ответ заведения темы: её id.
type topicResult struct {
	ThreadID int64 `json:"message_thread_id"`
}

// iconSticker — один значок из закрытого набора значков тем.
type iconSticker struct {
	Emoji         string `json:"emoji"`
	CustomEmojiID string `json:"custom_emoji_id"`
}

// apiUpdate — одно обновление опроса.
type apiUpdate struct {
	UpdateID int64       `json:"update_id"`
	Message  *apiMessage `json:"message"`
}

// apiMessage — сообщение в чате. Чат назван в кадре, потому что бот получает обновления всех
// своих чатов, а служба ведёт один.
type apiMessage struct {
	MessageID int      `json:"message_id"`
	ThreadID  int64    `json:"message_thread_id"`
	Chat      apiChat  `json:"chat"`
	From      *apiUser `json:"from"`
	Date      int64    `json:"date"`
	Text      string   `json:"text"`
	Caption   string   `json:"caption"`
	// ReplyTo — сообщение, на которое ответили. Вложенного реплая у него Telegram не присылает.
	ReplyTo *apiMessage `json:"reply_to_message"`
	// Quote — фрагмент исходного сообщения, выделенный при ответе.
	Quote *apiQuote `json:"quote"`
	// ForumTopicEdited — служебное сообщение о правке темы (`ForumTopicEdited`): тему поправили, и
	// текста у такого сообщения нет.
	ForumTopicEdited *apiTopicEdited `json:"forum_topic_edited"`
	// Остальные служебные сообщения о теме. Содержимое службе не нужно: важно, что это они.
	ForumTopicCreated  *struct{} `json:"forum_topic_created"`
	ForumTopicClosed   *struct{} `json:"forum_topic_closed"`
	ForumTopicReopened *struct{} `json:"forum_topic_reopened"`
	GeneralTopicHidden *struct{} `json:"general_forum_topic_hidden"`
	GeneralTopicShown  *struct{} `json:"general_forum_topic_unhidden"`
}

// isTopicService отвечает, служебное ли это сообщение о теме.
func (m *apiMessage) isTopicService() bool {
	return m.ForumTopicCreated != nil || m.ForumTopicEdited != nil ||
		m.ForumTopicClosed != nil || m.ForumTopicReopened != nil ||
		m.GeneralTopicHidden != nil || m.GeneralTopicShown != nil
}

// apiQuote — выделенный при ответе фрагмент (`TextQuote`).
type apiQuote struct {
	Text string `json:"text"`
}

// apiTopicEdited — правка темы. Поля необязательные: поля значка нет — эта правка значка не
// меняла, пустая строка — значок снят (https://core.telegram.org/bots/api#forumtopicedited). Имя
// правки службе не нужно, и поле его не читается.
type apiTopicEdited struct {
	IconCustomEmojiID *string `json:"icon_custom_emoji_id"`
}

// apiChat — чат сообщения.
type apiChat struct {
	ID int64 `json:"id"`
}

// apiUser — автор сообщения. Автора может не быть вовсе: анонимный админ и сообщение от имени
// группы приходят без него.
type apiUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}
