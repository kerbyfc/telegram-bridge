// Package protocol — кадры протокола моста по `contract.md`: один объект JSON в
// строке, запрос с `op` и `rid`, ответ с тем же `rid`, перечисления и коды ошибок. Имена полей
// заданы контрактом, поэтому у файла снят `tagliatelle`.
//
//nolint:tagliatelle // имена полей кадра заданы контрактом протокола и пишутся как в нём
package protocol

// Version — мажорная версия контракта, которая есть в каждом кадре.
const Version = 1

// Op — имя операции протокола.
type Op string

// Операции протокола «сессия — служба» и «служба — мод».
const (
	OpHello         Op = "hello"
	OpModeOn        Op = "mode.on"
	OpModeOff       Op = "mode.off"
	OpModeStatus    Op = "mode.status"
	OpServiceStatus Op = "service.status"
	OpTurnStart     Op = "turn.start"
	OpTurnEnd       Op = "turn.end"
	OpQuestionAsk   Op = "question.ask"
	OpDeliveryWait  Op = "delivery.wait"
	OpDeliveryAck   Op = "delivery.ack"
	OpSay           Op = "say"
	OpAgentMessage  Op = "agent.message"
	OpContextReport Op = "context.report"
	OpNameReport    Op = "name.report"
	OpBye           Op = "bye"
)

// Role — роль звонящего: носитель живости или короткий вызов.
type Role string

// Роли звонящего.
const (
	RoleSession Role = "session"
	RoleTool    Role = "tool"
)

// Kind — чем звонящий является внутри своей роли. Видов три: сессию держит только её средство,
// потому что сессии без мода не бывает.
type Kind string

// Виды звонящих.
const (
	KindMod     Kind = "mod"
	KindAdapter Kind = "adapter"
	KindTool    Kind = "tool"
)

// Client — клиент сессии, назвавшийся в приветствии. Клиенты равны: OpenCode работает с
// Telegram точно так же, как Claude Code.
type Client string

// Клиенты сессии.
const (
	ClientClaudeCode Client = "claude-code"
	ClientOpenCode   Client = "opencode"
)

// KindOf называет вид звонящего, которым клиент держит роль сессии: у Claude Code это мод, у
// OpenCode — плагин-адаптер. Знание живёт в протоколе, а не в команде: вид идёт в приветствии, и
// второе мерило развело бы кадр и то, чем его собирают. Незнакомый клиент отвечает `false` —
// подставлять ему вид нельзя, роль сессии занимает только мод или адаптер.
func KindOf(client Client) (Kind, bool) {
	switch client {
	case ClientClaudeCode:
		return KindMod, true
	case ClientOpenCode:
		return KindAdapter, true
	}

	return "", false
}

// Clients — клиенты сессии в порядке перечисления контракта: ими команда называет владельцу, что
// она понимает, не заводя второго списка.
func Clients() []Client {
	return []Client{ClientClaudeCode, ClientOpenCode}
}

// SayKind — вид сообщения, отправляемого в тему через `say`.
type SayKind string

// Виды сообщений `say`. Вывода исполненной команды среди них нет: командная кухня в теме не
// видна, а исход команды владелец видит отметкой на своём сообщении.
const (
	SayMessage SayKind = "message"
	SayNotice  SayKind = "notice"
)

// AckOutcome — исход подачи слова владельца в сессию.
type AckOutcome string

// Исходы подтверждения приёма.
const (
	AckAccepted AckOutcome = "accepted"
	AckRejected AckOutcome = "rejected"
)

// DeliveryKind — вид доставки, уезжающей соединению сессии.
type DeliveryKind string

// Виды доставки.
const (
	DeliveryOwnerMessage DeliveryKind = "owner-message"
	DeliveryClientCmd    DeliveryKind = "client-command"
)

// Mode — состояние режима сессии в ответах протокола.
type Mode string

// Состояния режима.
const (
	ModeOn  Mode = "on"
	ModeOff Mode = "off"
)

// AuthorKind — человек написал в тему или бот.
type AuthorKind string

// Виды автора сообщения.
const (
	AuthorHuman AuthorKind = "human"
	AuthorBot   AuthorKind = "bot"
)

// Option — вариант ответа на вопрос с вариантами. Нумерацию делает служба, разбора номера обратно
// в ключ нет: ответ владельца приходит доставкой, и номер понимает сам агент.
type Option struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// Author — кто написал в тему: провенанс записи ящика.
type Author struct {
	ID   int64      `json:"id"`
	Name string     `json:"name"`
	Kind AuthorKind `json:"kind"`
}

// TopicRef — тема, из которой пришла доставка.
type TopicRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Life — живость сессии, как её видит служба: единственный способ узнать снаружи, держит ли
// сессия своё соединение. Номера процессов в этом не участвуют нигде.
type Life string

// Состояния живости сессии.
const (
	LifeAlive    Life = "alive"
	LifeDoubtful Life = "doubtful"
	LifeDead     Life = "dead"
)

// SessionStatus — сессия в ответе самопроверки.
type SessionStatus struct {
	ID        string `json:"id"`
	Client    string `json:"client"`
	Mode      Mode   `json:"mode"`
	Life      Life   `json:"life"`
	TopicID   int64  `json:"topic_id"`
	TopicName string `json:"topic_name"`
	Inbox     int    `json:"inbox"`
}

// PollStatus — исход последнего опроса обновлений: им самопроверка и проверяет токен, группу и
// канал к Telegram, не делая второго вызова платформы.
type PollStatus struct {
	At   string `json:"at"`
	OK   bool   `json:"ok"`
	Code Code   `json:"code,omitempty"`
	Text string `json:"text,omitempty"`
}

// ServiceStatus — состояние самой службы: с какого времени идёт, как кончился последний опрос и
// что она знает о сессиях. Отдаётся только операцией `service.status`.
type ServiceStatus struct {
	Version   int             `json:"version"`
	StartedAt string          `json:"started_at"`
	Poll      PollStatus      `json:"poll"`
	Sessions  []SessionStatus `json:"sessions"`
}

// Error — отказ службы с кодом контракта.
type Error struct {
	Code Code   `json:"code"`
	Text string `json:"text"`
}

// Request — кадр запроса. Поля всех операций лежат одним плоским кадром: адресат читает те,
// которые знает его операция, и игнорирует остальные.
type Request struct {
	V   int    `json:"v"`
	Op  Op     `json:"op"`
	RID string `json:"rid"`

	Role    Role   `json:"role,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Session string `json:"session,omitempty"`
	Client  string `json:"client,omitempty"`

	Name string `json:"name,omitempty"`
	Text string `json:"text,omitempty"`

	Question string   `json:"question,omitempty"`
	Options  []Option `json:"options,omitempty"`

	DeliveryID string     `json:"delivery_id,omitempty"`
	Outcome    AckOutcome `json:"outcome,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	After      string     `json:"after,omitempty"`

	Percent int    `json:"percent,omitempty"`
	Turn    string `json:"turn,omitempty"`
}

// Response — кадр ответа. Поля, не относящиеся к операции, не пишутся.
type Response struct {
	V     int    `json:"v"`
	RID   string `json:"rid"`
	OK    bool   `json:"ok"`
	Error *Error `json:"error,omitempty"`

	Mode      Mode   `json:"mode,omitempty"`
	TopicID   int64  `json:"topic_id,omitempty"`
	TopicName string `json:"topic_name,omitempty"`
	Revived   bool   `json:"revived,omitempty"`
	Created   bool   `json:"created,omitempty"`

	MessageID  int   `json:"message_id,omitempty"`
	MessageIDs []int `json:"message_ids,omitempty"`

	Kind       DeliveryKind `json:"kind,omitempty"`
	DeliveryID string       `json:"delivery_id,omitempty"`
	Text       string       `json:"text,omitempty"`
	Provenance string       `json:"provenance,omitempty"`
	Author     *Author      `json:"author,omitempty"`
	Topic      *TopicRef    `json:"topic,omitempty"`

	Service *ServiceStatus `json:"service,omitempty"`
}

// Ok готовит удачный ответ на запрос: остальные поля дописываются присваиванием.
func Ok(rid string) Response {
	return Response{ //nolint:exhaustruct // поля ответа дописывает операция, которая его отдаёт
		V:   Version,
		RID: rid,
		OK:  true,
	}
}

// Fail готовит отказ с кодом контракта.
func Fail(rid string, code Code, text string) Response {
	return Response{ //nolint:exhaustruct // в отказе значимы только код и его текст
		V:     Version,
		RID:   rid,
		OK:    false,
		Error: &Error{Code: code, Text: text},
	}
}
