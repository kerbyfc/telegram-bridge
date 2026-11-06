package core_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/core"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// ownerID — id владельца в стенде.
const ownerID = 11111111

// Имена тем и тексты, которыми пользуются тесты.
const (
	topicAlpha     = "alpha"
	topicBeta      = "beta"
	topicInventory = "tg-inventory"
	textQuestion   = "вопрос"
)

// rig — стенд ядра: своё состояние в своём каталоге, поддельный клиент Bot API, короткие сроки.
type rig struct {
	t     *testing.T
	core  *core.Core
	api   *fakeAPI
	cfg   *config.Config
	store *store.Store
	jrnl  *store.Journal
	root  string
}

// hello называется службе и отдаёт соединение с её ответом.
func (r *rig) hello(session string, role protocol.Role, kind protocol.Kind) (*core.Conn, protocol.Response) {
	r.t.Helper()

	conn := r.core.NewConn()
	req := r.req(protocol.OpHello)
	req.Role = role
	req.Kind = string(kind)
	req.Session = session
	req.Client = "claude-code"

	return conn, r.do(conn, req)
}

// mod называется службе модом и включает режим на названной теме.
func (r *rig) mod(session, topic string) (*core.Conn, protocol.Response) {
	r.t.Helper()

	conn, resp := r.hello(session, protocol.RoleSession, protocol.KindMod)
	if !resp.OK {
		r.t.Fatalf("hello мода отказал: %+v", resp.Error)
	}

	req := r.req(protocol.OpModeOn)
	req.Name = topic
	on := r.do(conn, req)

	if !on.OK {
		r.t.Fatalf("mode.on отказал: %+v", on.Error)
	}

	return conn, on
}

// tool называется службе коротким вызовом: так работают тонкий клиент и чужой тулинг.
func (r *rig) tool(session string) *core.Conn {
	r.t.Helper()

	conn, resp := r.hello(session, protocol.RoleTool, protocol.KindTool)
	if !resp.OK {
		r.t.Fatalf("hello тонкого клиента отказал: %+v", resp.Error)
	}

	return conn
}

// do исполняет запрос на соединении.
func (r *rig) do(conn *core.Conn, req protocol.Request) protocol.Response {
	return r.core.Handle(context.Background(), conn, req)
}

// doCtx исполняет запрос с отменяемым сроком: долгий запрос доставки своего срока не имеет и
// висит, пока службе нечего отдать.
func (r *rig) doCtx(ctx context.Context, conn *core.Conn, req protocol.Request) protocol.Response {
	return r.core.Handle(ctx, conn, req)
}

// req готовит кадр запроса.
func (r *rig) req(op protocol.Op) protocol.Request {
	return protocol.Request{ //nolint:exhaustruct // поля дописывает сам тест
		V:   protocol.Version,
		Op:  op,
		RID: "r-" + string(op),
	}
}

// waitDelivery берёт доставку долгим запросом с коротким сроком: без доставки тест не висит.
func (r *rig) waitDelivery(conn *core.Conn) protocol.Response {
	r.t.Helper()

	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()

	req := r.req(protocol.OpDeliveryWait)

	return r.doCtx(ctx, conn, req)
}

// ack подтверждает приём доставки.
func (r *rig) ack(conn *core.Conn, id string, outcome protocol.AckOutcome, detail string) protocol.Response {
	r.t.Helper()

	req := r.req(protocol.OpDeliveryAck)
	req.DeliveryID = id
	req.Outcome = outcome
	req.Detail = detail

	return r.do(conn, req)
}

// ownerMessage подаёт в ядро сообщение владельца в тему и отдаёт его id.
func (r *rig) ownerMessage(topicID int64, text string) int {
	r.t.Helper()

	id := r.nextOwnerMessageID()
	r.core.Dispatch(context.Background(), botapi.Update{
		ID: 1,
		Message: &botapi.Message{
			ID:        id,
			TopicID:   topicID,
			Text:      text,
			From:      &botapi.User{ID: ownerID, Name: "Александр", IsBot: false, IsSelf: false},
			At:        time.Now(),
			ReplyTo:   nil,
			TopicEdit: nil,
		},
	})

	return id
}

// ownerReply подаёт в ядро реплай владельца в тему.
func (r *rig) ownerReply(topicID int64, text string, reply botapi.Reply) {
	r.t.Helper()
	r.core.Dispatch(context.Background(), botapi.Update{
		ID: 3,
		Message: &botapi.Message{
			ID:        r.nextOwnerMessageID(),
			TopicID:   topicID,
			Text:      text,
			From:      &botapi.User{ID: ownerID, Name: "Александр", IsBot: false, IsSelf: false},
			At:        time.Now(),
			ReplyTo:   &reply,
			TopicEdit: nil,
		},
	})
}

// strangerMessage подаёт сообщение не от владельца.
func (r *rig) strangerMessage(topicID int64, text string) {
	r.t.Helper()
	r.core.Dispatch(context.Background(), botapi.Update{
		ID: 2,
		Message: &botapi.Message{
			ID:        r.nextOwnerMessageID(),
			TopicID:   topicID,
			Text:      text,
			From:      &botapi.User{ID: 42, Name: "Прохожий", IsBot: false, IsSelf: false},
			At:        time.Now(),
			ReplyTo:   nil,
			TopicEdit: nil,
		},
	})
}

// selfMessage подаёт обновление о сообщении самой службы в тему.
func (r *rig) selfMessage(topicID int64, text string) {
	r.t.Helper()
	r.core.Dispatch(context.Background(), botapi.Update{
		ID: 4,
		Message: &botapi.Message{
			ID:        r.nextOwnerMessageID(),
			TopicID:   topicID,
			Text:      text,
			From:      &botapi.User{ID: 777, Name: "мост", IsBot: true, IsSelf: true},
			At:        time.Now(),
			ReplyTo:   nil,
			TopicEdit: nil,
		},
	})
}

// topicEdited подаёт служебное сообщение о правке темы владельцем: так приходит значок, сменённый
// руками. Платформа при этом уже держит новый значок — его ставит и подделка.
func (r *rig) topicEdited(topicID int64, edit botapi.TopicEdit) {
	r.t.Helper()

	if edit.IconChanged {
		r.api.setIcon(topicID, edit.Icon != "")
	}

	r.core.Dispatch(context.Background(), botapi.Update{
		ID: 5,
		Message: &botapi.Message{
			ID:        r.nextOwnerMessageID(),
			TopicID:   topicID,
			Text:      "",
			From:      &botapi.User{ID: ownerID, Name: "Александр", IsBot: false, IsSelf: false},
			At:        time.Now(),
			ReplyTo:   nil,
			TopicEdit: &edit,
		},
	})
}

// nextOwnerMessageID выдаёт следующий id сообщения владельца, не пересекаясь с сообщениями бота.
func (r *rig) nextOwnerMessageID() int {
	r.api.mu.Lock()
	defer r.api.mu.Unlock()

	r.api.nextMsg += 1000

	return r.api.nextMsg
}

// closeConn закрывает соединение.
func (r *rig) closeConn(conn *core.Conn) {
	r.t.Helper()
	r.core.CloseConn(context.Background(), conn)
}

// reap зовёт сторожа живости.
func (r *rig) reap() {
	r.t.Helper()
	r.core.Reap(context.Background())
}

// reopen перезапускает службу на том же состоянии: ядро поднимается заново, диск остаётся.
func (r *rig) reopen() {
	r.t.Helper()

	engine, err := core.New(r.cfg, r.api, r.store, r.jrnl)
	if err != nil {
		r.t.Fatalf("служба не поднялась на прежнем состоянии: %v", err)
	}

	r.t.Cleanup(engine.Close)

	r.core = engine
}

// inboxFiles отдаёт имена файлов ящика сессии.
func (r *rig) inboxFiles(session string) []string {
	r.t.Helper()

	entries, err := os.ReadDir(filepath.Join(r.root, "inbox", session))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		r.t.Fatalf("ящик не прочитан: %v", err)
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}

	return names
}

// journalEvents читает журнал службы и отдаёт события разобранными строками.
func (r *rig) journalEvents() []store.Entry {
	r.t.Helper()

	body, err := os.ReadFile(filepath.Join(r.root, "bridge.log"))
	if err != nil {
		r.t.Fatalf("журнал не прочитан: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	out := make([]store.Entry, 0, len(lines))

	for _, line := range lines {
		if line == "" {
			continue
		}

		var entry store.Entry

		err = json.Unmarshal([]byte(line), &entry)
		if err != nil {
			r.t.Fatalf("строка журнала не разобрана (%q): %v", line, err)
		}

		out = append(out, entry)
	}

	return out
}

// newRig готовит стенд со своим состоянием и короткими сроками.
func newRig(t *testing.T) *rig {
	t.Helper()

	root := t.TempDir()
	cfg := &config.Config{
		BotToken:         "токен",
		GroupID:          -100,
		OwnerID:          ownerID,
		StateRoot:        root,
		DefaultTopicName: "по-умолчанию",
		StatuslineRoot:   filepath.Join(root, "statusline"),
		SocketPath:       filepath.Join(root, "bridge.sock"),
		Proxy:            "",
		ReconnectGrace:   120 * time.Millisecond,
		PollTimeout:      20 * time.Millisecond,
		RetryPauses:      []time.Duration{time.Millisecond},
		ContextEditMin:   time.Millisecond,
		LogPath:          filepath.Join(root, "bridge.log"),
		LogMaxBytes:      config.DefaultLogMaxBytes,
		LogKeep:          config.DefaultLogKeep,
		RetryPausesRaw:   nil,
	}

	st, err := store.Open(root)
	if err != nil {
		t.Fatalf("состояние не открыто: %v", err)
	}

	jrnl, err := store.OpenJournal(cfg.Log(), cfg.LogMaxBytes, cfg.LogKeep)
	if err != nil {
		t.Fatalf("журнал не открыт: %v", err)
	}

	t.Cleanup(func() { _ = jrnl.Close() })

	api := newFakeAPI()

	engine, err := core.New(cfg, api, st, jrnl)
	if err != nil {
		t.Fatalf("ядро не поднялось: %v", err)
	}

	t.Cleanup(engine.Close)

	return &rig{t: t, core: engine, api: api, cfg: cfg, store: st, jrnl: jrnl, root: root}
}
