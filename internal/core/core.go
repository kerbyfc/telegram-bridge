// Package core — ядро службы моста: сессии, темы, очередь слов владельца и соединения сессий
// полями одного процесса под одним мьютексом. Гонок между хуком, поллером и модом здесь нет по
// устройству: писатель состояния один, и он же единственный владелец замка.
package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/statusline"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// reapInterval — как часто служба проверяет, не умерла ли сессия.
const reapInterval = 50 * time.Millisecond

// RefreshInterval — как часто служба переписывает вклад в статусную строку и результат зонда,
// пока у сессии включён режим. Это и есть такт записи, который обязан быть короче срока годности
// вклада: иначе режим, включённый дольше суток, уехал бы с экрана сам — `at + ttl` в прошлом, и
// сегмента нет молча, а у файла зонда свежесть мерится временем правки.
//
// Такт свой, а не поллера: он держится и когда Telegram не отвечает. Режим при мёртвой сети всё
// равно включён, и говорить об этом строке — правда, а не умолчание.
const RefreshInterval = time.Hour

// pollResult — исход последнего опроса обновлений.
type pollResult struct {
	// At — когда опрос кончился; нулевое время значит, что опроса ещё не было.
	At time.Time
	// OK — удачен ли он.
	OK bool
	// Code и Text — код контракта и причина отказа.
	Code protocol.Code
	Text string
}

// Core — ядро службы.
type Core struct {
	// mu держит всё состояние в памяти: сессии и их соединения, темы, ящик и ждущих доставку.
	mu sync.Mutex
	// topicMu удерживается на время работы с темой — создания, переименования и отправки, —
	// чтобы сетевой вызов не держал мьютекс состояния, а наименьший свободный суффикс не
	// доставался двум сессиям сразу.
	topicMu sync.Mutex

	cfg    *config.Config
	api    botapi.API
	store  *store.Store
	jrnl   *store.Journal
	status *statusline.Writer

	// startedAt — когда поднялся этот процесс службы. Его называет самопроверка: «служба идёт
	// с такого-то времени» — единственное, по чему виден перезапуск, которого никто не видел.
	startedAt time.Time
	// poll — исход последнего опроса обновлений. Это и есть живая проверка токена, группы и
	// канала к Telegram: удачный опрос значит, что все три на месте.
	poll pollResult

	// readyAt — момент, когда служба снова принимает соединения. Отсрочка на возврат
	// оборвавшейся сессии считается от него, а не от обрыва: иначе переустановка службы
	// хоронила бы разом все живые сессии.
	readyAt time.Time

	// resolveCtx живёт, пока служба не остановлена: от него запускаются команды разворота ссылок,
	// и остановка снимает их вместе с потомками. resolving считает горутины разворота, closed
	// запрещает новые: остановка дожидается всех, пока журнал ещё открыт.
	resolveCtx  context.Context //nolint:containedctx // срок жизни службы, а не запроса
	resolveStop context.CancelFunc
	resolving   sync.WaitGroup
	closed      bool

	topics   *store.Topics
	sessions map[string]*Session
	updates  *store.Updates
	nextSeq  uint64
	nextConn uint64
}

// Close останавливает разворот ссылок: команды снимаются вместе с потомками, их вывод в тему
// уже не идёт, а каждая брошенная ссылка оставляет строку журнала `link-dropped`. Возвращается,
// когда горутин разворота не осталось, поэтому журнал закрывается только после неё.
func (c *Core) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	c.resolveStop()
	c.resolving.Wait()
}

// Ready отмечает момент, когда служба начала принимать соединения.
func (c *Core) Ready() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.readyAt = time.Now()
}

// Reap хоронит сессии, чья смерть уже измерима. Вызывается сторожем живости и тестами.
func (c *Core) Reap(ctx context.Context) {
	c.mu.Lock()

	now := time.Now()
	dead := make([]*Session, 0, len(c.sessions))

	for _, sess := range c.sessions {
		if sess.state(now, c.cfg, c.readyAt) == lifeDead && !sess.disk.Buried {
			dead = append(dead, sess)
		}
	}

	c.mu.Unlock()

	for _, sess := range dead {
		c.bury(ctx, sess, "сессия ушла")
	}
}

// Watch держит сторожа живости и такт записи вклада, пока не кончится ctx.
func (c *Core) Watch(ctx context.Context) {
	reaper := time.NewTicker(reapInterval)
	defer reaper.Stop()

	refresher := time.NewTicker(RefreshInterval)
	defer refresher.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-reaper.C:
			c.Reap(ctx)
		case <-refresher.C:
			c.Refresh(time.Now())
		}
	}
}

// Config отдаёт настройки службы.
func (c *Core) Config() *config.Config {
	return c.cfg
}

// journal пишет строку журнала. Отказ записи журнала гасить нельзя — он значит, что служба
// больше не ведёт учёта, — поэтому он паника, а не тихий пропуск.
func (c *Core) journal(entry store.Entry) {
	err := c.jrnl.Write(entry)
	if err != nil {
		panic("журнал службы не пишется: " + err.Error())
	}
}

// mustSave пишет состояние на диск. Нарушенный инвариант состояния — паника: сторож не вправе
// продолжать работу, сделав вид, что записал.
func (c *Core) mustSave(err error) {
	if err != nil {
		panic("состояние службы не записано: " + err.Error())
	}
}

// session отдаёт сессию по id; её нет — nil.
func (c *Core) session(id string) *Session {
	return c.sessions[id]
}

// sessionOrNew отдаёт сессию по id, заводя привязку, если её ещё нет.
func (c *Core) sessionOrNew(req protocol.Request) *Session {
	sess, ok := c.sessions[req.Session]
	if ok {
		return sess
	}

	sess = newSession(store.Session{
		ID:        req.Session,
		Client:    req.Client,
		Mode:      false,
		TopicID:   0,
		TopicName: "",
		Buried:    false,
	})
	c.sessions[req.Session] = sess

	return sess
}

// sessionByTopic отдаёт сессию, привязанную к теме; привязки нет — nil.
func (c *Core) sessionByTopic(topicID int64) *Session {
	for _, sess := range c.sessions {
		if sess.disk.TopicID == topicID {
			return sess
		}
	}

	return nil
}

// New поднимает ядро на прочитанном состоянии: реестр тем, привязки сессий, ящик и смещение
// опроса. Негодное состояние — громкий отказ, а не пустое начало с чистого листа.
func New(cfg *config.Config, api botapi.API, st *store.Store, jrnl *store.Journal) (*Core, error) {
	topics, err := store.LoadTopics(st)
	if err != nil {
		return nil, fmt.Errorf("реестр тем не прочитан: %w", err)
	}

	bindings, err := store.LoadSessions(st)
	if err != nil {
		return nil, fmt.Errorf("привязки сессий не прочитаны: %w", err)
	}

	records, err := store.LoadInbox(st)
	if err != nil {
		return nil, fmt.Errorf("ящик не прочитан: %w", err)
	}

	updates, err := store.LoadUpdates(st)
	if err != nil {
		return nil, fmt.Errorf("смещение опроса не прочитано: %w", err)
	}

	resolveCtx, resolveStop := context.WithCancel(context.Background())

	core := &Core{
		resolveCtx:  resolveCtx,
		resolveStop: resolveStop,
		resolving:   sync.WaitGroup{},
		closed:      false,
		mu:          sync.Mutex{},
		topicMu:     sync.Mutex{},
		cfg:         cfg,
		api:         api,
		store:       st,
		jrnl:        jrnl,
		status:      statusline.New(cfg.Statusline()),
		startedAt:   time.Now(),
		poll:        pollResult{At: time.Time{}, OK: false, Code: "", Text: ""},
		readyAt:     time.Now(),
		topics:      topics,
		sessions:    make(map[string]*Session, len(bindings)),
		updates:     updates,
		nextSeq:     1,
		nextConn:    1,
	}

	for _, binding := range bindings {
		core.sessions[binding.ID] = newSession(*binding)
	}

	for _, rec := range records {
		sess, ok := core.sessions[rec.Session]
		if !ok {
			resolveStop()

			return nil, errOrphanRecord(rec.Session, rec.ID)
		}

		sess.inbox = append(sess.inbox, rec)

		if rec.Seq >= core.nextSeq {
			core.nextSeq = rec.Seq + 1
		}
	}

	core.restoreContributions()

	return core, nil
}
