package core_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// Имена методов поддельного клиента, на которые тест ставит отказ платформы.
const (
	methodSendMessage = "SendMessage"
	methodCreateTopic = "CreateTopic"
	methodRenameTopic = "RenameTopic"
	methodSetReaction = "SetReaction"
	methodGetUpdates  = "GetUpdates"
)

// sentMessage — отправленное поддельным клиентом сообщение темы.
type sentMessage struct {
	topicID int64
	id      int
	text    string
}

// fakeAPI — поддельный клиент Bot API: настоящий пишется этапом клиента, ядру он нужен только
// портом. Всё состояние под своим мьютексом, потому что ядро зовёт его из многих горутин.
type fakeAPI struct {
	mu sync.Mutex

	nextTopic int64
	nextMsg   int

	sent      []sentMessage
	created   []string
	renamed   []string
	names     map[int64]string
	inMode    map[int64]bool
	pinned    map[int64]int
	edits     map[int]string
	reactions map[int]botapi.Mark
	actions   int
	// iconCalls — сколько раз ядро звало переключение значка: лишний вызов видно по нему.
	iconCalls int
	// iconRefusals — сколько ближайших переключений значка платформа отобьёт отказом.
	iconRefusals int
	// polled — смещения, с которыми ядро звало опрос, по порядку.
	polled []int64

	// noTopic — темы, следующий вызов по которым отвечает «тема не найдена».
	noTopic map[int64]int
	// failures — отказы платформы, поставленные тестом на ближайшие вызовы метода по порядку.
	failures map[string][]error
	// hold держит отправку, пока тест его не закроет: так проверяется застрявший сетевой вызов.
	hold chan struct{}

	// polls — когда начинался каждый долгий опрос: по ним видны паузы поллера между отказами.
	polls []time.Time

	updates chan botapi.Update
}

// SendMessage отправляет сообщение в тему.
func (f *fakeAPI) SendMessage(_ context.Context, topicID int64, text string) (int, error) {
	f.mu.Lock()
	hold := f.hold
	f.mu.Unlock()

	if hold != nil {
		<-hold
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	err := f.planned(methodSendMessage)
	if err != nil {
		return 0, err
	}

	err = f.refuse(topicID)
	if err != nil {
		return 0, err
	}

	// Текст длиннее предела сообщения клиент Bot API отбивает сам, до платформы: подделка держит
	// тот же порт.
	if botapi.TextUnits(text) > botapi.MessageLimit {
		return 0, botapi.Refuse(protocol.CodeTelegramRefused, "текст длиннее предела сообщения")
	}

	f.nextMsg++
	f.sent = append(f.sent, sentMessage{topicID: topicID, id: f.nextMsg, text: text})

	return f.nextMsg, nil
}

// EditMessageText правит текст сообщения.
func (f *fakeAPI) EditMessageText(_ context.Context, messageID int, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Правка сообщения, которого в теме нет (его унесло удаление темы), отбивается отказом:
	// на этом стоит восстановление закреплённой строки.
	if !f.known(messageID) {
		return botapi.Refuse(protocol.CodeTelegramRefused, "сообщение не найдено")
	}

	if botapi.TextUnits(text) > botapi.MessageLimit {
		return botapi.Refuse(protocol.CodeTelegramRefused, "правка длиннее предела сообщения")
	}

	f.edits[messageID] = text

	return nil
}

// known отвечает, отправляла ли подделка такое сообщение.
func (f *fakeAPI) known(messageID int) bool {
	for _, msg := range f.sent {
		if msg.id == messageID {
			return true
		}
	}

	return false
}

// CreateTopic заводит тему. Имя темы держится таким, каким его держит платформа: клиент Bot API
// режет длинное по её пределу.
func (f *fakeAPI) CreateTopic(_ context.Context, name string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	err := f.planned(methodCreateTopic)
	if err != nil {
		return 0, err
	}

	name = botapi.TopicName(name, "")

	f.nextTopic++
	f.created = append(f.created, name)
	f.names[f.nextTopic] = name

	return f.nextTopic, nil
}

// RenameTopic переименовывает тему.
func (f *fakeAPI) RenameTopic(_ context.Context, topicID int64, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	err := f.planned(methodRenameTopic)
	if err != nil {
		return err
	}

	err = f.refuse(topicID)
	if err != nil {
		return err
	}

	name = botapi.TopicName(name, "")

	// Telegram отбивает переименование в то же имя — контракт, «Чего Telegram не даёт».
	if f.names[topicID] == name {
		return botapi.Refuse(protocol.CodeTelegramRefused, "тема уже так называется")
	}

	f.renamed = append(f.renamed, name)
	f.names[topicID] = name

	return nil
}

// SetTopicInMode переключает значок темы. Значок, который уже стоит, платформа отбивает отказом
// «ничего не изменилось», а клиент Bot API отвечает на него успехом «уже так»: подделка держит
// порт, а не платформу, и лишний вызов видно по счёту вызовов, а не по отказу.
func (f *fakeAPI) SetTopicInMode(_ context.Context, topicID int64, inMode bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.iconCalls++

	err := f.refuse(topicID)
	if err != nil {
		return err
	}

	if f.iconRefusals > 0 {
		f.iconRefusals--

		return botapi.Refuse(protocol.CodeTelegramRefused, "Bad Request: not enough rights to manage topics")
	}

	f.inMode[topicID] = inMode

	return nil
}

// SetReaction ставит отметку доставки на сообщении владельца.
func (f *fakeAPI) SetReaction(_ context.Context, messageID int, mark botapi.Mark) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	err := f.planned(methodSetReaction)
	if err != nil {
		return err
	}

	f.reactions[messageID] = mark

	return nil
}

// PinMessage закрепляет сообщение в теме.
func (f *fakeAPI) PinMessage(_ context.Context, topicID int64, messageID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pinned[topicID] = messageID

	return nil
}

// SendChatAction держит действие чата.
func (f *fakeAPI) SendChatAction(_ context.Context, _ int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.actions++

	return nil
}

// GetUpdates отдаёт обновления, положенные тестом, иначе пустой долгий опрос. Поставленный
// тестом отказ уходит сразу, без ожидания: так отвечает опрос, которому отказала платформа.
func (f *fakeAPI) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]botapi.Update, error) {
	f.mu.Lock()
	f.polled = append(f.polled, offset)
	f.polls = append(f.polls, time.Now())
	err := f.planned(methodGetUpdates)
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("опрос отменён: %w", ctx.Err())
	case upd := <-f.updates:
		return append([]botapi.Update{upd}, f.drain()...), nil
	case <-timer.C:
		return nil, nil
	}
}

// drain добирает обновления, уже лежащие в очереди: пачка приходит пачкой, как у Bot API.
func (f *fakeAPI) drain() []botapi.Update {
	var rest []botapi.Update

	for {
		select {
		case upd := <-f.updates:
			rest = append(rest, upd)
		default:
			return rest
		}
	}
}

// planned отдаёт отказ, поставленный тестом на ближайший вызов метода, и снимает его с очереди.
// Звать под мьютексом подделки.
func (f *fakeAPI) planned(method string) error {
	queue := f.failures[method]
	if len(queue) == 0 {
		return nil
	}

	f.failures[method] = queue[1:]

	return queue[0]
}

// refuse отдаёт запланированный отказ «тема не найдена».
func (f *fakeAPI) refuse(topicID int64) error {
	left, ok := f.noTopic[topicID]
	if ok && left > 0 {
		f.noTopic[topicID] = left - 1

		return botapi.Refuse(protocol.CodeNoTopic, "тема не найдена")
	}

	return nil
}

// texts отдаёт тексты всех сообщений темы.
func (f *fakeAPI) texts(topicID int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.sent))

	for _, msg := range f.sent {
		if msg.topicID == topicID {
			out = append(out, msg.text)
		}
	}

	return out
}

// nameOf отдаёт имя темы, каким его держит платформа.
func (f *fakeAPI) nameOf(topicID int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.names[topicID]
}

// inModeOf отдаёт состояние значка темы.
func (f *fakeAPI) inModeOf(topicID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.inMode[topicID]
}

// setIcon ставит теме значок мимо службы — так его меняет владелец руками: `true` — значок
// режима, `false` — значок снят.
func (f *fakeAPI) setIcon(topicID int64, inMode bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.inMode[topicID] = inMode
}

// refuseIcon ставит отказ платформы на ближайшие переключения значка.
func (f *fakeAPI) refuseIcon(times int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.iconRefusals = times
}

// firstPoll отдаёт смещение первого опроса; опроса ещё не было — ложь.
func (f *fakeAPI) firstPoll() (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.polled) == 0 {
		return 0, false
	}

	return f.polled[0], true
}

// iconCallCount отдаёт, сколько раз ядро звало переключение значка.
func (f *fakeAPI) iconCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.iconCalls
}

// mark отдаёт последнюю отметку на сообщении.
func (f *fakeAPI) mark(messageID int) botapi.Mark {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.reactions[messageID]
}

// createdNames отдаёт имена заведённых тем.
func (f *fakeAPI) createdNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.created...)
}

// renamedNames отдаёт имена, на которые темы переименовывались.
func (f *fakeAPI) renamedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.renamed...)
}

// chatActions отдаёт число вызовов действия чата.
func (f *fakeAPI) chatActions() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.actions
}

// plan ставит отказ «тема не найдена» на ближайшие вызовы по теме.
func (f *fakeAPI) plan(topicID int64, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.noTopic[topicID] = times
}

// fail ставит отказы платформы на ближайшие вызовы метода: каждый вызов забирает по одному, по
// порядку, а кончились отказы — метод снова отвечает как обычно.
func (f *fakeAPI) fail(method string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failures[method] = append(f.failures[method], errs...)
}

// pollTimes отдаёт моменты начала долгих опросов.
func (f *fakeAPI) pollTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]time.Time(nil), f.polls...)
}

// newFakeAPI готовит поддельный клиент.
func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		mu:        sync.Mutex{},
		nextTopic: 0,
		nextMsg:   0,
		sent:      nil,
		created:   nil,
		renamed:   nil,
		names:     map[int64]string{},
		inMode:    map[int64]bool{},
		pinned:    map[int64]int{},
		edits:     map[int]string{},
		reactions: map[int]botapi.Mark{},
		actions:   0,
		iconCalls: 0,
		polled:    nil,

		iconRefusals: 0,
		noTopic:      map[int64]int{},
		failures:     map[string][]error{},
		hold:         nil,
		polls:        nil,
		updates:      make(chan botapi.Update, 16),
	}
}
