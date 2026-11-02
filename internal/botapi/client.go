package botapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// BaseURL — адрес Bot API.
const BaseURL = "https://api.telegram.org"

// MessageLimit — предел сообщения Telegram. Считается он не знаками, а единицами UTF-16: эмодзи
// вне основного набора занимает две, и текст из таких эмодзи, уложившийся в предел знаков,
// платформа отбила бы отказом. Предел и счёт единиц живут здесь, в слое, который знает
// платформу: мерило у нарезки, у правки закреплённой строки и у самой отправки одно, иначе на шве
// между двумя мерилами рождается отказ, которого никто не ждёт.
const MessageLimit = 4096

// messageLimit — то же значение для расчёта пределов внутри клиента.
const messageLimit = MessageLimit

// topicNameLimit — предел имени темы в знаках (`createForumTopic`: 1–128); длиннее — режется с
// многоточием.
const topicNameLimit = 128

// DefaultSendGap — пауза между отправками в чат: подряд идущие отправки Telegram считает флудом,
// и ловить флуд-лимит на своём же ходу незачем. Пауза одна на клиент, потому что чат один — темы
// живут в одной супергруппе, и предел частоты у неё общий. Названа в контракте строкой таблицы
// таймаутов: темп видимого владельцу потока реплик молча в коде не живёт.
//
// Отметки доставки, закреп, правка текста и действие чата этой паузой не идут: у них свои
// пределы платформы, и ход службы на них не стоит.
const DefaultSendGap = time.Second

// callTimeout — срок обычного вызова Bot API. Долгий опрос в него не входит: у него свой срок.
const callTimeout = 30 * time.Second

// updatesLimit — размер пачки долгого опроса, предел платформы. Назван в кадре опроса, потому
// что от него считается предел читаемого ответа.
const updatesLimit = 100

// pollMargin — надбавка к сроку долгого опроса: ответ приходит по истечении названного Telegram
// срока, и рвать вызов ровно на нём значило бы не дождаться ответа никогда.
const pollMargin = 10 * time.Second

// iconInMode — значок темы, которым видно «сессия этой темы в режиме». Какой это значок — вид
// темы, он решается этапом вида; клиент даёт операцию и держит выбор одним значением, чтобы
// операцию было чем исполнить.
const iconInMode = "🤖"

// Значки отметок доставки — выбор владельца; ядро знает только состояния, значки знает клиент.
// Каждый значок назван здесь ровно один раз: замена — правка одной строки (и её ожидания в
// тесте `TestMarksCarryOwnerEmoji`). Перечень Telegram закрытый, и не всякий значок в нём есть:
// по решению владельца песочных часов платформа в реакциях бота не разрешает — ни ⌛, ни ⏳, —
// поэтому придержанное слово носит 😴.
const (
	emojiReceived = "👀"
	emojiHeld     = "😴"
	emojiAccepted = "🫡"
)

// ErrOptions — клиент Bot API не собрался из названных значений.
var ErrOptions = errors.New("клиент Bot API не собран")

// Options — чем собирается клиент Bot API. Настройки службы целиком сюда не приходят: клиенту
// нужны только названные значения, и собирает его команда запуска.
type Options struct {
	// Token — токен бота; его началом бот узнаёт сам себя.
	Token string
	// GroupID — id супергруппы с темами: единственный чат, который служба ведёт.
	GroupID int64
	// Proxy — явный прокси до Bot API из своего ключа настроек; пусто — прямое соединение.
	// Переменные прокси окружения транспорт не читает вовсе.
	Proxy string
	// RetryPauses — паузы повтора сетевых отказов по порядку.
	RetryPauses []time.Duration
	// SendGap — пауза между отправками в чат.
	SendGap time.Duration
	// BaseURL — адрес Bot API; пусто — `BaseURL`.
	BaseURL string
}

// Client — настоящий клиент Bot API: тот, что ходит в Telegram.
type Client struct {
	http    *http.Client
	base    string
	token   string
	groupID int64
	selfID  int64

	retryPauses []time.Duration
	sendGap     time.Duration

	mu   sync.Mutex
	icon string

	sendMu   sync.Mutex
	lastSend time.Time
}

// SelfID отдаёт id бота, прочитанный из токена: им самопроверка называет бота, не спрашивая
// платформу. Токен и есть ответ, и вызова для этого не нужно.
func (c *Client) SelfID() int64 {
	return c.selfID
}

// SendMessage отправляет текст в тему и возвращает id сообщения, дождавшись своей очереди
// отправки.
//
// Текст уходит одним сообщением, целиком: режет его звонящий — `Cut`, тем же пределом и тем же
// счётом единиц. Своей резки здесь нет нарочно: кусков у одной отправки было бы несколько, а id
// наружу уходил бы один, и `message_ids` реплики теряли бы середину нарезки. Текст длиннее
// предела — отказ с названной причиной: молча порезать его значит отдать звонящему id, за
// которым стоит не весь текст.
func (c *Client) SendMessage(ctx context.Context, topicID int64, text string) (int, error) {
	units := TextUnits(text)
	if units > MessageLimit {
		return 0, Refuse(protocol.CodeTelegramRefused, fmt.Sprintf(
			"текст длиннее предела сообщения: %d единиц UTF-16 против %d, звонящий его не нарезал",
			units, MessageLimit))
	}

	err := c.sendTurn(ctx)
	if err != nil {
		return 0, err
	}

	var result messageResult

	params := sendMessageParams{ChatID: c.groupID, ThreadID: topicID, Text: text, Preview: previewOff()}

	err = c.call(ctx, "sendMessage", params, &result)
	if err != nil {
		return 0, err
	}

	return result.MessageID, nil
}

// EditMessageText правит текст уже отправленного сообщения на месте. Правка у службы одна —
// закреплённая строка темы, — и предел у неё тот же, что у отправки: мерится он тем же счётом
// единиц UTF-16, поэтому слишком длинная правка отбивается здесь, а не на платформе.
//
// Текст, который уже стоит, — успех «уже так», а не отказ: правку, ничего не изменившую, платформа
// отбивает отказом `message is not modified`, а служба просила ровно этот текст.
func (c *Client) EditMessageText(ctx context.Context, messageID int, text string) error {
	units := TextUnits(text)
	if units > MessageLimit {
		return Refuse(protocol.CodeTelegramRefused, fmt.Sprintf(
			"правка длиннее предела сообщения: %d единиц UTF-16 против %d", units, MessageLimit))
	}

	params := editTextParams{ChatID: c.groupID, MessageID: messageID, Text: text, Preview: previewOff()}

	err := c.call(ctx, "editMessageText", params, nil)
	if errors.Is(err, errNotModified) {
		return nil
	}

	return err
}

// CreateTopic заводит тему и возвращает её id.
func (c *Client) CreateTopic(ctx context.Context, name string) (int64, error) {
	var result topicResult

	params := topicNameParams{ChatID: c.groupID, Name: cutName(name)}

	err := c.call(ctx, "createForumTopic", params, &result)
	if err != nil {
		return 0, err
	}

	return result.ThreadID, nil
}

// RenameTopic переименовывает тему; тем же именем Telegram отвечает отказом, поэтому зовётся она
// только на изменение — имя сверяется со своим состоянием.
func (c *Client) RenameTopic(ctx context.Context, topicID int64, name string) error {
	params := topicRenameParams{ChatID: c.groupID, ThreadID: topicID, Name: cutName(name)}

	return c.call(ctx, "editForumTopic", params, nil)
}

// SetTopicInMode переключает значок темы. Снятие — пустое значение значка: им Telegram возвращает
// теме её значок по умолчанию.
//
// Значок, который уже стоит, — успех «уже так», а не отказ: правку, ничего не изменившую, платформа
// отбивает отказом `TOPIC_NOT_MODIFIED`, а служба просила ровно то состояние, в котором тема и так
// стоит. Так бывает, когда значок сменили руками и запись реестра о нём устарела.
func (c *Client) SetTopicInMode(ctx context.Context, topicID int64, inMode bool) error {
	icon := ""

	if inMode {
		resolved, err := c.modeIcon(ctx)
		if err != nil {
			return err
		}

		icon = resolved
	}

	params := topicIconParams{ChatID: c.groupID, ThreadID: topicID, Icon: icon}

	err := c.call(ctx, "editForumTopic", params, nil)
	if errors.Is(err, errNotModified) {
		return nil
	}

	return err
}

// SetReaction ставит сообщению владельца отметку доставки. Список реакции всегда из одного
// значка: реакция у бота одна, и новая отметка вытесняет прежнюю.
func (c *Client) SetReaction(ctx context.Context, messageID int, mark Mark) error {
	params := reactionParams{
		ChatID:    c.groupID,
		MessageID: messageID,
		Reaction:  []reaction{{Type: "emoji", Emoji: markEmoji(mark)}},
	}

	return c.call(ctx, "setMessageReaction", params, nil)
}

// PinMessage закрепляет сообщение в теме. Тема в кадре закрепа не названа и названа быть не
// может: Telegram закрепляет сообщение там, где оно лежит, а лежит оно в своей теме.
func (c *Client) PinMessage(ctx context.Context, _ int64, messageID int) error {
	params := pinParams{ChatID: c.groupID, MessageID: messageID, Silent: true}

	return c.call(ctx, "pinChatMessage", params, nil)
}

// SendChatAction держит действие чата в теме. Действие гаснет само и в истории не остаётся,
// поэтому держится оно повторными вызовами, а не одним.
func (c *Client) SendChatAction(ctx context.Context, topicID int64, action string) error {
	params := chatActionParams{ChatID: c.groupID, ThreadID: topicID, Action: action}

	return c.call(ctx, "sendChatAction", params, nil)
}

// GetUpdates — долгий опрос обновлений со смещения.
//
// Повторов здесь нет: опрос повторяет сам поллер — он держит паузы повтора и пишет каждый отказ
// в журнал, а второй слой повторов ждал бы названные контрактом паузы дважды.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	params := getUpdatesParams{
		Offset:  offset,
		Timeout: int(timeout.Seconds()),
		Limit:   updatesLimit,
		Allowed: []string{"message"},
	}

	var raw []apiUpdate

	pollCtx, cancel := context.WithTimeout(ctx, timeout+pollMargin)
	defer cancel()

	err := c.callOnce(pollCtx, "getUpdates", params, &raw)
	if err != nil {
		return nil, err
	}

	updates := make([]Update, 0, len(raw))
	for _, item := range raw {
		updates = append(updates, Update{ID: item.UpdateID, Message: c.message(item.Message)})
	}

	return updates, nil
}

// sendTurn держит очередь отправки: между отправками в чат проходит не меньше паузы. Куски
// нарезанной реплики — те же отправки, поэтому пауза держит и их. Отсчёт идёт от начала прошлой
// отправки: Telegram считает пришедшие сообщения, а не затраченное на них время.
func (c *Client) sendTurn(ctx context.Context) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	err := hold(ctx, c.sendGap-time.Since(c.lastSend))
	if err != nil {
		return err
	}

	c.lastSend = time.Now()

	return nil
}

// modeIcon разрешает значок «в режиме» в id пользовательского эмодзи: набор значков тем закрытый,
// и другого способа взять id у Bot API нет. Разрешается однажды — набор платформы в жизни службы
// не меняется. Значка нет в наборе — отказ с названной причиной, а не другой значок молча.
func (c *Client) modeIcon(ctx context.Context) (string, error) {
	c.mu.Lock()
	known := c.icon
	c.mu.Unlock()

	if known != "" {
		return known, nil
	}

	var stickers []iconSticker

	err := c.call(ctx, "getForumTopicIconStickers", emptyParams{}, &stickers)
	if err != nil {
		return "", err
	}

	for _, sticker := range stickers {
		if sticker.Emoji != iconInMode {
			continue
		}

		c.mu.Lock()
		c.icon = sticker.CustomEmojiID
		c.mu.Unlock()

		return sticker.CustomEmojiID, nil
	}

	return "", Refuse(protocol.CodeTelegramRefused, "в наборе значков тем Telegram нет "+iconInMode)
}

// message переводит сообщение Bot API в сообщение порта.
//
// Сообщение чужого чата службе не адресовано вовсе: бот получает обновления всех своих чатов, а
// служба ведёт один. До ядра такое обновление доходит пустым, а не выброшенным целиком, — иначе
// смещение опроса встало бы на нём и тот же чужой чат возвращался бы вечно.
func (c *Client) message(msg *apiMessage) *Message {
	if msg == nil || msg.Chat.ID != c.groupID {
		return nil
	}

	return &Message{
		ID:        msg.MessageID,
		TopicID:   msg.ThreadID,
		Text:      msg.Text,
		From:      c.author(msg.From),
		At:        time.Unix(msg.Date, 0),
		TopicEdit: topicEdit(msg.ForumTopicEdited),
	}
}

// author переводит автора сообщения. Автора может не быть вовсе — анонимный админ и сообщение от
// имени группы приходят без него, и это нулевой указатель, а не пустое имя. Своего бота от чужого
// отличает клиент: он знает себя по токену.
func (c *Client) author(user *apiUser) *User {
	if user == nil {
		return nil
	}

	name := user.FirstName
	if name == "" {
		name = user.Username
	}

	return &User{ID: user.ID, Name: name, IsBot: user.IsBot, IsSelf: user.ID == c.selfID}
}

// NewClient собирает клиент Bot API. Обязательные значения проверяются здесь: без них клиент не
// собирается и служба не поднимается наполовину.
func NewClient(opts Options) (*Client, error) {
	if opts.Token == "" {
		return nil, fmt.Errorf("%w: нет токена бота", ErrOptions)
	}

	if opts.GroupID == 0 {
		return nil, fmt.Errorf("%w: нет id группы", ErrOptions)
	}

	selfID, err := botID(opts.Token)
	if err != nil {
		return nil, err
	}

	client, err := newHTTP(opts.Proxy)
	if err != nil {
		return nil, err
	}

	base := opts.BaseURL
	if base == "" {
		base = BaseURL
	}

	return &Client{
		http:        client,
		base:        strings.TrimSuffix(base, "/"),
		token:       opts.Token,
		groupID:     opts.GroupID,
		selfID:      selfID,
		retryPauses: opts.RetryPauses,
		sendGap:     opts.SendGap,
		mu:          sync.Mutex{},
		icon:        "",
		sendMu:      sync.Mutex{},
		lastSend:    time.Time{},
	}, nil
}

// botID достаёт id бота из токена: он стоит в его начале до двоеточия. Спрашивать его у
// платформы незачем — токен и есть ответ, и знать себя клиенту нужно с первого же обновления.
func botID(token string) (int64, error) {
	head, _, found := strings.Cut(token, ":")
	if !found {
		return 0, fmt.Errorf("%w: в токене бота нет двоеточия", ErrOptions)
	}

	id, err := strconv.ParseInt(head, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: начало токена не id бота: %w", ErrOptions, err)
	}

	return id, nil
}

// markEmoji называет значок отметки доставки. Отметок три, и четвёртой не бывает: отметка не из
// перечня — нарушенный инвариант, на котором служба падает, а не ставит что попало.
func markEmoji(mark Mark) string {
	switch mark {
	case MarkReceived:
		return emojiReceived
	case MarkHeld:
		return emojiHeld
	case MarkAccepted:
		return emojiAccepted
	default:
		panic("отметка доставки вне перечня: " + string(mark))
	}
}

// Cut режет текст по пределу сообщения, по порядку. Единица предела — единица UTF-16, а границы
// кусков идут по знакам: пара UTF-16 одного знака не рвётся. Режет звонящий, а не отправка: у
// каждого куска свой id, и наружу уходят все.
func Cut(text string) []string {
	limit := MessageLimit

	total := TextUnits(text)
	if total <= limit {
		return []string{text}
	}

	parts := make([]string, 0, total/limit+1)
	part := make([]rune, 0, limit)
	units := 0

	for _, one := range text {
		size := utf16.RuneLen(one)
		if units+size > limit {
			parts = append(parts, string(part))
			part = part[:0]
			units = 0
		}

		part = append(part, one)
		units += size
	}

	if len(part) > 0 {
		parts = append(parts, string(part))
	}

	return parts
}

// TextUnits считает длину текста в единицах UTF-16 — в них Telegram считает предел сообщения.
// Мерило одно у всех, кто имеет дело с пределом: у нарезки, у правки закреплённой строки и у
// отправки.
func TextUnits(text string) int {
	units := 0
	for _, one := range text {
		units += utf16.RuneLen(one)
	}

	return units
}

// topicEdit переводит служебное сообщение о правке темы: поля значка нет — правка его не меняла,
// пустая строка — значок снят.
func topicEdit(edited *apiTopicEdited) *TopicEdit {
	if edited == nil {
		return nil
	}

	if edited.IconCustomEmojiID == nil {
		return &TopicEdit{IconChanged: false, Icon: ""}
	}

	return &TopicEdit{IconChanged: true, Icon: *edited.IconCustomEmojiID}
}

// previewOff собирает выключенный предпросмотр ссылок — решение владельца о виде темы.
func previewOff() previewOption {
	return previewOption{Disabled: true}
}

// TopicName — имя темы в пределах платформы: длинное режется с многоточием. Хвост `suffix` — номер
// второй темы того же имени — остаётся целым, а режется начало: иначе две темы одного длинного имени
// звались бы в Telegram одинаково, а реестр держал бы их разными. Режет ядро, когда имя входит в
// реестр, и клиент — на каждом вызове платформы с именем: мерило у них одно, и уже резаное имя
// второй резкой не меняется.
func TopicName(name, suffix string) string {
	room := topicNameLimit - utf8.RuneCountInString(suffix)
	if room < 1 {
		panic(fmt.Sprintf("хвост имени темы %q не оставляет места имени: предел %d знаков", suffix, topicNameLimit))
	}

	runes := []rune(name)
	if len(runes) <= room {
		return name + suffix
	}

	return string(runes[:room-1]) + "…" + suffix
}

// cutName режет имя темы по пределу платформы с многоточием.
func cutName(name string) string {
	return TopicName(name, "")
}
