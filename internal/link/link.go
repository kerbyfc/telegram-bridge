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
	// Retry — пауза перед новым соединением, когда прежнее пропало. Попытки идут, пока служба не
	// вернётся: потеря связи подкоманду не кончает.
	Retry time.Duration
	// Absent — сколько службы может не быть молча. Штатный перезапуск укладывается в него, и
	// владелец о нём не слышит; дольше — средство сессии получает событие `absent` один раз на
	// пропажу.
	Absent time.Duration
	// Steady — сколько соединение должно прожить, чтобы считаться вернувшимся: короче — служба
	// падает сразу после приветствия, и это та же пропажа, а не новая. Прожив его, соединение
	// говорит событие `steady`.
	Steady time.Duration
	// Hello — срок ответа на приветствие: служба, которая приняла соединение и молчит, — та же
	// потеря связи, а не вечное ожидание.
	Hello time.Duration
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

	// ack — подтверждение приёма, которое средство сессии дало, когда связи со службой не было:
	// оно уходит службе первым кадром вернувшегося соединения, раньше долгого запроса, — иначе
	// служба подала бы то же слово второй раз.
	ack *Ack
	// deferred будит `resume`, который уже ждёт подтверждения, когда подтверждение отложено:
	// иначе отложенное после начала ожидания лежало бы до следующего обрыва.
	deferred chan struct{}
}

// maxBacklog — сколько реплик агента подкоманда копит, пока связи со службой нет. Больше —
// старейшая теряется, и средство сессии говорит об этом владельцу событием `undelivered`.
const maxBacklog = 64

// Link держит соединение сессии со службой и местный вход для средства сессии.
type Link struct {
	opts Options

	out sync.Mutex

	mu      sync.Mutex
	client  *Client
	pending *handoff
	after   string
	front   string
	// frontURL и frontToken — вход на loopback и его токен; пусты, где такого входа нет.
	frontURL   string
	frontToken string

	stop     chan struct{}
	stopOnce sync.Once

	// leaving закрывается, когда средство сессии сказало `bye`, а ответа ещё нет. Похороны на
	// службе кончают долгий запрос отказом раньше, чем приходит ответ на `bye`, и этот отказ —
	// конец сессии, а не вытеснение: подкоманда ждёт ответа на `bye`, а не рвёт соединение.
	leaving   chan struct{}
	leaveOnce sync.Once

	// backlog — реплики агента, ждущие связи со службой, в порядке прихода; flushing — их
	// отдаёт вернувшееся соединение, и новая реплика встаёт за ними, а не обгоняет их.
	backlog  []reply
	seq      uint64
	flushing bool
	kick     chan struct{}

	// lostAt — когда пропала связь со службой; пусто, пока пропажи нет. upAt — когда открылось
	// последнее соединение. Их и absent держит только цикл соединений `hold`, поэтому мьютекса у
	// них нет.
	lostAt time.Time
	upAt   time.Time
	// absent — о пропаже этой длины средству сессии уже сказано событием `absent`.
	absent bool
}

// Run держит соединение сессии, пока жив ctx или пока средство сессии не сказало `bye`. Конец ctx —
// остановка подкоманды извне: что она значит для сессии, решает `stopped`.
func (l *Link) Run(ctx context.Context) error {
	fronts, err := endpoint.Front(ctx)
	if err != nil {
		return err //nolint:wrapcheck // причина названа адресом и площадкой внутри endpoint
	}

	defer fronts.Close()

	err = l.serveFronts(fronts)
	if err != nil {
		return err
	}

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

	go l.flusher(runCtx)

	err = l.hold(runCtx)

	l.abandon()

	return err
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
// хоронит, и мод обязан вернуться в пределах отсрочки возврата. Сокет пропал, служба не отвечает,
// соединение оборвано — всё это потеря связи: подкоманда ждёт паузу и звонит снова в том же
// процессе, сколько бы службы ни было. Кончает её только отказ, после которого новое соединение
// делу не поможет.
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

		l.lose(err)

		select {
		case <-ctx.Done():
			return nil
		case <-l.stop:
			return nil
		case <-time.After(l.opts.Retry):
		}
	}
}

// lose отмечает потерю связи. Строка в поток ошибок и событие `lost` идут один раз на пропажу, а
// не на каждую попытку: служба, которой нет минуту, иначе сыпала бы ими каждую паузу. Пропажа
// дольше `Absent` — уже не штатный перезапуск, и средство сессии узнаёт о ней событием `absent`.
func (l *Link) lose(err error) {
	// Соединение, прожившее `Steady`, кончило прежнюю пропажу; короче — служба падает сразу после
	// приветствия, и пропажа длится: иначе петля падений никогда не дошла бы до `absent`.
	if !l.upAt.IsZero() {
		if time.Since(l.upAt) >= l.opts.Steady {
			l.lostAt = time.Time{}
			l.absent = false
		}

		l.upAt = time.Time{}
	}

	if l.lostAt.IsZero() {
		l.lostAt = time.Now()

		l.note("соединение со службой потеряно, звоню снова: " + err.Error())
		l.emit(Event{Event: EventLost, Text: err.Error()}) //nolint:exhaustruct // поля события задаёт его вид

		return
	}

	gone := time.Since(l.lostAt)
	if l.absent || gone < l.opts.Absent {
		return
	}

	l.absent = true

	// Владельцу — что канала пока нет и что делать ничего не надо; причина попытки идёт в поток
	// ошибок, откуда средство сессии кладёт её в журнал отладки.
	text := fmt.Sprintf("службы нет %d с, жду её и подключусь сам", int(gone.Seconds()))

	l.note(text + ": " + err.Error())
	l.emit(Event{Event: EventAbsent, Text: text}) //nolint:exhaustruct // поля события задаёт его вид
}

// regain отмечает, что соединение открылось. Пропажа считается оконченной не здесь, а когда
// соединение проживёт `Steady` (`lose` на следующей потере и событие `steady`).
func (l *Link) regain() {
	l.upAt = time.Now()

	if l.lostAt.IsZero() {
		return
	}

	l.note(fmt.Sprintf("соединение со службой открылось через %d с после потери", int(time.Since(l.lostAt).Seconds())))
}

// steady говорит средству сессии, что соединение прожило `Steady`: связь вернулась всерьёз, и
// сказанное владельцу о пропаже можно снять.
func (l *Link) steady(ctx context.Context, client *Client) {
	timer := time.NewTimer(l.opts.Steady)
	defer timer.Stop()

	select {
	case <-timer.C:
		l.emit(Event{Event: EventSteady}) //nolint:exhaustruct // поля события задаёт его вид
	case <-client.Done():
	case <-ctx.Done():
	}
}

// attach открывает одно соединение сессии: приветствие, затем единственный долгий запрос.
func (l *Link) attach(ctx context.Context) error {
	client, err := Dial(ctx, l.opts.Endpoint)
	if err != nil {
		return err
	}

	defer client.Close()

	greeted, cancel := context.WithTimeout(ctx, l.opts.Hello)
	defer cancel()

	resp, err := client.Call(greeted, l.hello())
	if err != nil {
		if ctx.Err() == nil && greeted.Err() != nil {
			return fmt.Errorf("%w: служба не ответила на приветствие за %s", ErrLinkLost, l.opts.Hello)
		}

		return err
	}

	if !resp.OK {
		return refusal(resp)
	}

	l.adopt(client)
	l.regain()

	defer l.release()

	url, token := l.Loopback()

	l.emit(Event{ //nolint:exhaustruct // поля события задаёт его вид
		Event: EventReady, Socket: l.Front(), URL: url, Token: token, Mode: resp.Mode,
		TopicID: resp.TopicID, TopicName: resp.TopicName, Revived: resp.Revived,
	})

	go l.steady(ctx, client)

	l.wake()

	err = l.resume(ctx, client)
	if err != nil {
		return err
	}

	return l.pump(ctx, client)
}

// resume доводит доставку, выданную прежнему соединению: подтверждение, данное без связи, уходит
// службе первым, а не данное ещё — ждётся. Долгий запрос идёт только после него: служба подала бы
// то же слово второй раз, а мод подал бы его в сессию дважды.
func (l *Link) resume(ctx context.Context, client *Client) error {
	l.mu.Lock()
	taken := l.pending
	l.mu.Unlock()

	if taken == nil {
		return nil
	}

	for {
		l.mu.Lock()

		ack := taken.ack

		l.mu.Unlock()

		if ack != nil {
			return l.forward(ctx, client, taken, *ack)
		}

		// Подтверждения ещё нет: оно придёт по этому соединению само (`confirm`) или будет
		// отложено гонкой с ним (`postpone`) — тогда ожидание просыпается и отдаёт его отсюда.
		select {
		case <-taken.done:
			return taken.failure
		case <-taken.deferred:
		case <-client.Done():
			return fmt.Errorf("%w: подтверждения приёма дождаться не на чем", ErrLinkLost)
		case <-l.stop:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("ожидание подтверждения приёма прервано: %w", ctx.Err())
		}
	}
}

// forward отдаёт отложенное подтверждение приёма вернувшимся соединением.
func (l *Link) forward(ctx context.Context, client *Client, taken *handoff, ack Ack) error {
	resp, err := client.Call(ctx, ackRequest(ack))
	if err != nil {
		return err
	}

	if !resp.OK {
		l.breakOff(taken, refusal(resp))

		return taken.failure
	}

	l.note("подтверждение приёма " + ack.DeliveryID + " отдано службе после возврата связи")
	l.settle(ack, taken)

	return nil
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
			if l.departing() {
				l.await(ctx)

				return nil
			}

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

	return l.awaitAck(ctx, client, taken)
}

// awaitAck ждёт подтверждения приёма выданной доставки. Пропало соединение — доставка остаётся
// выданной: подтверждение мода дойдёт службе вернувшимся соединением (`resume`).
func (l *Link) awaitAck(ctx context.Context, client *Client, taken *handoff) error {
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

	// Связи нет — подтверждение не теряется: оно уйдёт службе первым кадром вернувшегося
	// соединения, раньше долгого запроса.
	if client == nil {
		return l.postpone(ack, taken), nil
	}

	resp, err := client.Call(ctx, ackRequest(ack))
	if errors.Is(err, ErrLinkLost) {
		return l.postpone(ack, taken), nil
	}

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

// postpone откладывает подтверждение приёма до возврата связи и отвечает средству сессии удачей:
// подтверждение принято подкомандой и дойдёт до службы, а слово владельца не подастся дважды.
func (l *Link) postpone(ack Ack, taken *handoff) protocol.Response {
	l.mu.Lock()

	if l.pending == taken {
		taken.ack = &ack
	}

	l.mu.Unlock()

	select {
	case taken.deferred <- struct{}{}:
	default:
	}

	l.note("подтверждение приёма " + ack.DeliveryID + " уйдёт службе, когда связь вернётся")

	return protocol.Ok("")
}

// farewell говорит службе, что сессия кончилась, и кончает подкоманду: соединение сессии больше
// не нужно. Хоронит сессию служба сама, в тему о этом не пишется ничего.
func (l *Link) farewell(ctx context.Context) (protocol.Response, error) {
	client, err := l.linked()
	if err != nil {
		return protocol.Response{}, err
	}

	l.leave()

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

// release забывает кончившееся соединение. Выданная доставка остаётся: мод её уже подаёт, и его
// подтверждение уйдёт службе вернувшимся соединением, а не потеряется — иначе служба подала бы то
// же слово второй раз.
func (l *Link) release() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.client = nil
}

// expect запоминает выданную доставку и отдаёт её, чтобы ждать подтверждения приёма.
func (l *Link) expect(id string) *handoff {
	taken := &handoff{
		id: id, done: make(chan struct{}), once: sync.Once{}, failure: nil,
		ack: nil, deferred: make(chan struct{}, 1),
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.pending = taken

	return taken
}

// handed сверяет, что подтверждение пришло на выданную доставку, и отдаёт соединение сессии; без
// связи соединения нет, и подтверждение откладывается.
func (l *Link) handed(id string) (*Client, *handoff, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

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

// serveFronts запоминает адреса местных входов и начинает их слушать. Вход площадки токена не
// требует: сокет и канал закрыты правами до владельца. Вход на loopback открыт всякому процессу
// машины, поэтому принимает запрос только с токеном этого запуска.
func (l *Link) serveFronts(fronts endpoint.Fronts) error {
	handler := front{link: l}

	address, token := "", ""

	if fronts.Loopback != nil {
		var err error

		token, err = newToken()
		if err != nil {
			return err
		}

		address = "http://" + fronts.Loopback.Addr().String()
	}

	l.setFront(fronts.Local.Addr().String(), address, token)

	go func() { _ = http.Serve(fronts.Local, handler) }()

	if fronts.Loopback != nil {
		go func() { _ = http.Serve(fronts.Loopback, guard(token, handler)) }()
	}

	return nil
}

// setFront запоминает адреса местных входов: адрес площадки и, на Windows, вход на loopback с его
// токеном.
func (l *Link) setFront(socket, address, token string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.front = socket
	l.frontURL = address
	l.frontToken = token
}

// Loopback отдаёт вход на loopback и его токен. Пусто на площадках, где такого входа нет, и пока
// вход не поднят.
func (l *Link) Loopback() (string, string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.frontURL, l.frontToken
}

// halt отмечает, что подкоманда кончилась по слову средства сессии.
func (l *Link) halt() {
	l.stopOnce.Do(func() { close(l.stop) })
}

// leave отмечает, что средство сессии сказало `bye` и ответа ещё нет.
func (l *Link) leave() {
	l.leaveOnce.Do(func() { close(l.leaving) })
}

// departing отвечает, сказан ли уже `bye`.
func (l *Link) departing() bool {
	select {
	case <-l.leaving:
		return true
	default:
		return false
	}
}

// await ждёт конца `bye`: соединение нужно ему до ответа.
func (l *Link) await(ctx context.Context) {
	select {
	case <-l.stop:
	case <-ctx.Done():
	}
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
		opts:       opts,
		out:        sync.Mutex{},
		mu:         sync.Mutex{},
		client:     nil,
		pending:    nil,
		after:      "",
		front:      "",
		frontURL:   "",
		frontToken: "",
		backlog:    nil,
		seq:        0,
		flushing:   false,
		kick:       make(chan struct{}, 1),
		upAt:       time.Time{},
		stop:       make(chan struct{}),
		stopOnce:   sync.Once{},
		leaving:    make(chan struct{}),
		leaveOnce:  sync.Once{},
		lostAt:     time.Time{},
		absent:     false,
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

// ackRequest собирает кадр подтверждения приёма.
func ackRequest(ack Ack) protocol.Request {
	return protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		Op: protocol.OpDeliveryAck, DeliveryID: ack.DeliveryID,
		Outcome: ack.Outcome, Detail: ack.Detail,
	}
}
