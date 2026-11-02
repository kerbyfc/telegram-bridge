package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/endpoint"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// Options — чем живёт соединение сессии: адрес службы, сессия, клиент и куда писать события.
type Options struct {
	// Endpoint — адрес службы: путь сокета на macOS, имя канала на Windows.
	Endpoint string
	// Session — id сессии, как клиент кладёт его в свой вход.
	Session string
	// Client — клиент сессии, как его называет приветствие контракта.
	Client protocol.Client
	// Kind — вид звонящего: роль сессии держит только мод или адаптер.
	Kind protocol.Kind
	// Retry — пауза перед новым соединением, когда прежнее пропало.
	Retry time.Duration
	// StopFarewell — срок `bye` на остановке подкоманды: служба молчит — подкоманда всё равно
	// выходит в этот срок, и выход клиента ею не висит.
	StopFarewell time.Duration
	// Events — куда уходят события средству сессии: его стандартная выдача.
	Events io.Writer
	// Log — куда уходят строки о жизни соединения: стандартный поток ошибок.
	Log io.Writer
}

// handoff — доставка, выданная средству сессии. Следующий `delivery.wait` ждёт подтверждения
// приёма: отдав доставку, мод зовёт `delivery.ack` и сразу же новый `delivery.wait`.
type handoff struct {
	id   string
	done chan struct{}
	once sync.Once

	// failure — отказ службы на подтверждении приёма. Соединение сессии при этом больше не её,
	// и ждать на нём нечего: отказ кончает долгий запрос, а не ставит его на вечное ожидание.
	failure error
}

// Link держит соединение сессии со службой и местный вход для средства сессии.
type Link struct {
	opts Options

	out sync.Mutex

	mu      sync.Mutex
	client  *Client
	pending *handoff
	after   string
	front   string

	stop     chan struct{}
	stopOnce sync.Once
}

// Run держит соединение сессии, пока жив ctx или пока средство сессии не сказало `bye`. Конец ctx —
// остановка подкоманды извне: что она значит для сессии, решает `stopped`.
func (l *Link) Run(ctx context.Context) error {
	listener, cleanup, err := endpoint.Front(ctx)
	if err != nil {
		return err //nolint:wrapcheck // причина названа адресом и площадкой внутри endpoint
	}

	defer func() {
		_ = listener.Close()

		cleanup()
	}()

	l.setFront(listener.Addr().String())

	go func() { _ = http.Serve(listener, front{link: l}) }()

	// Соединение живёт своим ctx, а не ctx остановки: остановка у клиента, чей процесс и есть
	// сессия, сначала говорит службе `bye` по ещё открытому соединению и только потом его гасит.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()

	// Слово средства сессии о конце сессии кончает и долгий запрос: ждать доставку больше
	// незачем, а другого способа снять ожидание у протокола нет.
	go func() {
		select {
		case <-l.stop:
		case <-ctx.Done():
			// Срок `bye` свой: ctx остановки уже кончился.
			l.stopped(context.WithoutCancel(ctx))
		case <-runCtx.Done():
		}

		cancel()
	}()

	return l.hold(runCtx)
}

// stopped решает, что значит остановка подкоманды извне. У Claude Code процесс клиента и есть
// сессия, а дочерние процессы мода клиент гасит на выходе раньше, чем зовёт `session.end`, — `bye`
// мода с `session.end` приходит уже в мёртвый сокет. Поэтому остановку подкоманда читает концом
// сессии и говорит `bye` сама. У OpenCode сервер живёт дальше сессии, и плагин гасит соединение
// без `bye` нарочно — служба хоронит такую сессию отсрочкой возврата.
func (l *Link) stopped(ctx context.Context) {
	switch l.opts.Client {
	case protocol.ClientClaudeCode:
		l.farewellOnStop(ctx)
	case protocol.ClientOpenCode:
	default:
		panic("соединение сессии незнакомого клиента: " + string(l.opts.Client))
	}
}

// farewellOnStop говорит службе `bye` на остановке. Не дошло — служба похоронит сессию отсрочкой
// возврата, а причина уходит строкой в поток ошибок: у остановленной подкоманды другого канала нет.
func (l *Link) farewellOnStop(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, l.opts.StopFarewell)
	defer cancel()

	resp, err := l.farewell(ctx)

	switch {
	case err != nil:
		l.note("конец сессии на остановке службе не сказан: " + err.Error())
	case !resp.OK:
		l.note("конец сессии на остановке служба отбила: " + refusal(resp).Error())
	}
}

// Front отдаёт адрес местного входа: по нему средство сессии подтверждает приём. Пусто, пока
// вход не поднят.
func (l *Link) Front() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.front
}

// hold открывает соединение заново, пока оно теряется: перезапуск самой службы сессий не
// хоронит, и мод обязан вернуться в пределах отсрочки возврата. Отказ, после которого новое
// соединение делу не поможет, кончает подкоманду.
func (l *Link) hold(ctx context.Context) error {
	for {
		err := l.attach(ctx)

		// Отмена ctx и слово средства сессии о конце сессии отказом не считаются: соединение
		// кончилось потому, что кончилась сессия.
		if ctx.Err() != nil || l.halted() {
			return nil //nolint:nilerr // конец сессии — не отказ соединения
		}

		if errors.Is(err, ErrVersion) || errors.Is(err, ErrRefused) {
			l.fault(err)

			return err
		}

		l.note("соединение со службой потеряно: " + err.Error())
		l.emit(Event{Event: EventLost, Text: err.Error()}) //nolint:exhaustruct // поля события задаёт его вид

		select {
		case <-ctx.Done():
			return nil
		case <-l.stop:
			return nil
		case <-time.After(l.opts.Retry):
		}
	}
}

// attach открывает одно соединение сессии: приветствие, затем единственный долгий запрос.
func (l *Link) attach(ctx context.Context) error {
	client, err := Dial(ctx, l.opts.Endpoint)
	if err != nil {
		return err
	}

	defer client.Close()

	resp, err := client.Call(ctx, l.hello())
	if err != nil {
		return err
	}

	if !resp.OK {
		return refusal(resp)
	}

	l.adopt(client)

	defer l.release()

	l.emit(Event{ //nolint:exhaustruct // поля события задаёт его вид
		Event: EventReady, Socket: l.Front(), Mode: resp.Mode,
		TopicID: resp.TopicID, TopicName: resp.TopicName, Revived: resp.Revived,
	})

	return l.pump(ctx, client)
}

// pump ведёт доставку: долгий запрос, выдача средству сессии, подтверждение приёма и сразу
// новый долгий запрос.
func (l *Link) pump(ctx context.Context, client *Client) error {
	for {
		if l.halted() {
			return nil
		}

		resp, err := client.Call(ctx, l.waiting())
		if err != nil {
			return err
		}

		if !resp.OK {
			return refusal(resp)
		}

		err = l.hand(ctx, client, resp)
		if err != nil {
			return err
		}
	}
}

// hand отдаёт доставку средству сессии и ждёт подтверждения приёма. Отправка доставки приёмом не
// считается: пока мод не подтвердил, слово владельца лежит в ящике, и владелец видит на нём, что
// оно придержано.
func (l *Link) hand(ctx context.Context, client *Client, resp protocol.Response) error {
	taken := l.expect(resp.DeliveryID)

	l.emit(Event{ //nolint:exhaustruct // поля события задаёт его вид
		Event: EventDelivery, Kind: resp.Kind, DeliveryID: resp.DeliveryID,
		Text: resp.Text, Provenance: resp.Provenance, Author: resp.Author, Topic: resp.Topic,
	})

	select {
	case <-taken.done:
		return taken.failure
	case <-client.Done():
		return fmt.Errorf("%w: подтверждения приёма дождаться не на чем", ErrLinkLost)
	case <-l.stop:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ожидание подтверждения приёма прервано: %w", ctx.Err())
	}
}

// confirm проводит подтверждение приёма от средства сессии в службу. Курсор сдвигается только по
// принятой доставке: честь ставит подтверждённый приём, а не выдача.
func (l *Link) confirm(ctx context.Context, ack Ack) (protocol.Response, error) {
	err := ack.validate()
	if err != nil {
		return protocol.Response{}, err
	}

	client, taken, err := l.handed(ack.DeliveryID)
	if err != nil {
		return protocol.Response{}, err
	}

	resp, err := client.Call(ctx, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		Op: protocol.OpDeliveryAck, DeliveryID: ack.DeliveryID,
		Outcome: ack.Outcome, Detail: ack.Detail,
	})
	if err != nil {
		return protocol.Response{}, err
	}

	// Отказ службы на подтверждении уходит средству сессии как есть и кончает соединение: его
	// отбивает только та служба, для которой это соединение больше не сессии, и ждать на нём
	// нечего — вытеснение соединение не закрывает.
	if !resp.OK {
		l.breakOff(taken, refusal(resp))

		return resp, nil
	}

	l.settle(ack, taken)

	return resp, nil
}

// farewell говорит службе, что сессия кончилась, и кончает подкоманду: соединение сессии больше
// не нужно. Хоронит сессию служба сама, в тему о этом не пишется ничего.
func (l *Link) farewell(ctx context.Context) (protocol.Response, error) {
	client, err := l.linked()
	if err != nil {
		return protocol.Response{}, err
	}

	defer l.halt()

	//nolint:exhaustruct // кадр заполняется по операции
	resp, err := client.Call(ctx, protocol.Request{Op: protocol.OpBye})
	if err != nil {
		return protocol.Response{}, err
	}

	return resp, nil
}

// hello — первый кадр соединения: версия, роль, вид звонящего, сессия и клиент. Роль сессии
// занимает только мод или адаптер: сессии без мода не бывает.
func (l *Link) hello() protocol.Request {
	return protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		Op:      protocol.OpHello,
		Role:    protocol.RoleSession,
		Kind:    string(l.opts.Kind),
		Session: l.opts.Session,
		Client:  string(l.opts.Client),
	}
}

// waiting собирает долгий запрос доставки. `after` — память о последней принятой доставке: по
// ней служба не отдаёт дважды то, что мод уже принял.
func (l *Link) waiting() protocol.Request {
	l.mu.Lock()
	defer l.mu.Unlock()

	//nolint:exhaustruct // кадр заполняется по операции
	return protocol.Request{Op: protocol.OpDeliveryWait, After: l.after}
}

// adopt запоминает соединение сессии: по нему идут подтверждение приёма и конец сессии.
func (l *Link) adopt(client *Client) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.client = client
}

// release забывает кончившееся соединение вместе с выданной доставкой: подтверждать приём
// некуда, и запись осталась в ящике — служба подаст её заново новому соединению.
func (l *Link) release() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.client = nil
	l.pending = nil
}

// expect запоминает выданную доставку и отдаёт её, чтобы ждать подтверждения приёма.
func (l *Link) expect(id string) *handoff {
	taken := &handoff{id: id, done: make(chan struct{}), once: sync.Once{}, failure: nil}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.pending = taken

	return taken
}

// handed находит соединение сессии и сверяет, что подтверждение пришло на выданную доставку.
func (l *Link) handed(id string) (*Client, *handoff, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.client == nil {
		return nil, nil, ErrNoLink
	}

	if l.pending == nil || l.pending.id != id {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotHanded, id)
	}

	return l.client, l.pending, nil
}

// linked находит соединение сессии.
func (l *Link) linked() (*Client, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.client == nil {
		return nil, ErrNoLink
	}

	return l.client, nil
}

// settle закрывает выданную доставку: следующий долгий запрос идёт сразу за подтверждением.
func (l *Link) settle(ack Ack, taken *handoff) {
	l.mu.Lock()

	if ack.Outcome == protocol.AckAccepted {
		l.after = ack.DeliveryID
	}

	if l.pending == taken {
		l.pending = nil
	}

	l.mu.Unlock()

	taken.once.Do(func() { close(taken.done) })
}

// breakOff кончает ожидание подтверждения отказом службы: долгий запрос не возобновляется, а
// отказ доходит и до средства сессии событием, и до кода возврата подкоманды.
func (l *Link) breakOff(taken *handoff, failure error) {
	l.mu.Lock()

	taken.failure = failure

	if l.pending == taken {
		l.pending = nil
	}

	l.mu.Unlock()

	taken.once.Do(func() { close(taken.done) })
}

// setFront запоминает адрес местного входа.
func (l *Link) setFront(socket string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.front = socket
}

// halt отмечает, что подкоманда кончилась по слову средства сессии.
func (l *Link) halt() {
	l.stopOnce.Do(func() { close(l.stop) })
}

// halted отвечает, кончилась ли подкоманда по слову средства сессии.
func (l *Link) halted() bool {
	select {
	case <-l.stop:
		return true
	default:
		return false
	}
}

// emit отдаёт событие средству сессии одной строкой.
func (l *Link) emit(event Event) {
	body, err := json.Marshal(event)
	if err != nil {
		panic("событие средству сессии не собрано: " + err.Error())
	}

	l.out.Lock()
	defer l.out.Unlock()

	_, _ = fmt.Fprintln(l.opts.Events, string(body))
}

// fault говорит средству сессии об отказе, после которого подкоманда не работает: о расхождении
// версий контракта владелец узнаёт в терминале, а не по пропавшему каналу.
func (l *Link) fault(err error) {
	event := Event{Event: EventError, Text: err.Error()} //nolint:exhaustruct // поля события задаёт его вид

	var refused RefusedError
	if errors.As(err, &refused) {
		event.Code = refused.Code
		event.Text = refused.Text
	}

	l.emit(event)
}

// note пишет строку о жизни соединения в свой поток ошибок: средство сессии кладёт её в журнал
// отладки, а в тему служебных строк о жизни сессии не уходит ни одной.
func (l *Link) note(text string) {
	if l.opts.Log == nil {
		return
	}

	_, _ = fmt.Fprintln(l.opts.Log, "telegram-bridge link: "+text)
}

// New собирает соединение сессии по настройкам.
func New(opts Options) *Link {
	return &Link{
		opts:     opts,
		out:      sync.Mutex{},
		mu:       sync.Mutex{},
		client:   nil,
		pending:  nil,
		after:    "",
		front:    "",
		stop:     make(chan struct{}),
		stopOnce: sync.Once{},
	}
}

// refusal переводит отказ службы в ошибку подкоманды. Расхождение версий и негодное приветствие
// повторным соединением не лечатся: первое — чужая версия контракта, второе — вытесненное
// соединение сессии, и новое отбирало бы сессию у того мода, который её уже держит.
func refusal(resp protocol.Response) error {
	if resp.Error == nil {
		panic("отказ службы без кода: кадр протокола нарушен")
	}

	reason := ErrRefused
	if resp.Error.Code == protocol.CodeVersion {
		reason = ErrVersion
	}

	return RefusedError{Code: resp.Error.Code, Text: resp.Error.Text, reason: reason}
}
