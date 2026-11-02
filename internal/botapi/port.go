// Package botapi — Bot API: порт, которым ядро службы пользуется, и клиент, который ходит в
// Telegram. Порт — интерфейс `API` с типами обновлений; клиент — `Client`, он знает токен,
// группу, прокси, пределы платформы и значки, которые видит владелец. В тестах ядра порт
// подменяется поддельной реализацией, в тестах клиента Bot API подменяется сервером.
package botapi

import (
	"context"
	"errors"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// API — всё, что ядру нужно от Bot API. Один метод — один вызов платформы.
type API interface {
	// SendMessage отправляет текст в тему и возвращает id сообщения.
	SendMessage(ctx context.Context, topicID int64, text string) (int, error)
	// EditMessageText правит текст уже отправленного сообщения на месте.
	EditMessageText(ctx context.Context, messageID int, text string) error
	// CreateTopic заводит тему и возвращает её id.
	CreateTopic(ctx context.Context, name string) (int64, error)
	// RenameTopic переименовывает тему; тем же именем Telegram отвечает отказом.
	RenameTopic(ctx context.Context, topicID int64, name string) error
	// CloseTopic закрывает тему. Закрытая уже — успех «уже так».
	CloseTopic(ctx context.Context, topicID int64) error
	// ReopenTopic открывает закрытую тему снова. Открытая уже — успех «уже так».
	ReopenTopic(ctx context.Context, topicID int64) error
	// SetTopicInMode переключает значок темы: грубое состояние «сессия этой темы в режиме».
	// Каким значком это показано, решает клиент Bot API, а не ядро.
	SetTopicInMode(ctx context.Context, topicID int64, inMode bool) error
	// SetReaction ставит сообщению владельца отметку доставки. Реакция у бота одна на сообщение,
	// поэтому новая отметка вытесняет прежнюю — отметки сменяют друг друга, а не копятся; какой
	// значок означает каждую, выбирает владелец, и знает это клиент Bot API.
	SetReaction(ctx context.Context, messageID int, mark Mark) error
	// PinMessage закрепляет сообщение в теме.
	PinMessage(ctx context.Context, topicID int64, messageID int) error
	// SendChatAction держит действие чата («печатает») в теме.
	SendChatAction(ctx context.Context, topicID int64, action string) error
	// GetUpdates — долгий опрос обновлений со смещения.
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
}

// Mark — отметка доставки на сообщении владельца.
type Mark string

// Отметки доставки: три состояния, которые владелец читает на своём же сообщении. Больше одной
// сразу не бывает — реакция у бота одна, и новая отметку вытесняет прежнюю.
const (
	// MarkReceived — принял мост: обновление вернулось опросом и записано в ящик.
	MarkReceived Mark = "received"
	// MarkHeld — придержано: у агента этого слова нет, оно ждёт в ящике.
	MarkHeld Mark = "held"
	// MarkAccepted — принял агент: сессия подтвердила приём.
	MarkAccepted Mark = "accepted"
)

// Update — одно обновление опроса. Обновление одно: сообщение в тему. Смена реакции входом не
// является — в теме видно только текст, и значки ставит сама служба отметками доставки.
type Update struct {
	ID      int64
	Message *Message
}

// FromSelf отвечает, своё ли это обновление: своё же сообщение в тему, вернувшееся тем же
// опросом. Кто такой «свой», знает клиент Bot API — у него токен, — и он отмечает это в авторе;
// ядру остаётся не разбирать такие обновления вовсе.
func (u *Update) FromSelf() bool {
	return u.Message != nil && u.Message.From != nil && u.Message.From.IsSelf
}

// Message — сообщение в теме группы.
type Message struct {
	ID      int
	TopicID int64
	Text    string
	From    *User
	At      time.Time
	// ReplyTo — на что ответил владелец; nil — сообщение не реплай. Реплаем на служебное сообщение о
	// теме Telegram метит обычные сообщения темы, и реплаем это не считается.
	ReplyTo *Reply
	// TopicEdit — служебное сообщение о правке темы; nil — сообщение не о правке. Правка темы — не
	// слово владельца: текста у неё нет, и сессии она не адресована.
	TopicEdit *TopicEdit
}

// Reply — сообщение, на которое ответил владелец.
type Reply struct {
	// Text — текст исходного сообщения или его подпись; пусто — ни того ни другого нет.
	Text string
	// Quote — фрагмент исходного сообщения, выделенный при ответе; пусто — ничего не выделено.
	Quote string
}

// TopicEdit — правка темы, сделанная мимо службы: её служебное сообщение приходит тем же опросом.
// Из правки службе нужен значок — его состояние держит реестр тем.
type TopicEdit struct {
	// IconChanged — правка меняла значок; иначе значок этой правкой не трогался.
	IconChanged bool
	// Icon — значок после правки: id пользовательского эмодзи, пусто — значок снят.
	Icon string
}

// User — автор сообщения или реакции. Отсутствие автора (анонимный админ, сообщение от имени
// группы) обозначается нулевым указателем, а не пустым именем. Своего бота от чужого отличает
// клиент Bot API: он знает, кто он, а ядро — нет.
type User struct {
	ID     int64
	Name   string
	IsBot  bool
	IsSelf bool
}

// Error — отказ Bot API с кодом контракта. Сетевой отказ повторяется по паузам, отказ платформы
// не повторяется впустую, а пропавшая тема чинится заведением темы заново.
type Error struct {
	Code       protocol.Code
	Text       string
	RetryAfter time.Duration
	// Cause — причина отказа, когда она есть: отказ соединения, останов службы или «тема уже
	// такая». Код контракта причину не заменяет — по ней ядро отличает штатный останов от мёртвой
	// сети, а переключение значка — «уже так» от отказа.
	Cause error
}

// Error делает отказ платформы обычной ошибкой Go.
func (e *Error) Error() string {
	return string(e.Code) + ": " + e.Text
}

// Unwrap отдаёт причину отказа: `errors.Is(err, context.Canceled)` обязан проходить сквозь код
// контракта, иначе штатный останов службы не отличить от отказа сети.
func (e *Error) Unwrap() error {
	return e.Cause
}

// Refuse собирает отказ платформы без срока повтора и без причины-ошибки.
func Refuse(code protocol.Code, text string) *Error {
	return &Error{Code: code, Text: text, RetryAfter: 0, Cause: nil}
}

// Code достаёт код контракта из ошибки порта; у ошибки не от порта код — `telegram-refused`.
func Code(err error) protocol.Code {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}

	return protocol.CodeTelegramRefused
}

// IsNoTopic отвечает, пропала ли тема: единственный способ узнать об её удалении владельцем.
func IsNoTopic(err error) bool {
	return Code(err) == protocol.CodeNoTopic
}

// RetryAfter достаёт срок повтора у отказа с паузой.
func RetryAfter(err error) time.Duration {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}

	return 0
}
