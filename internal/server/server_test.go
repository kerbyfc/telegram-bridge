package server_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/core"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/resolve/resolvetest"
	"github.com/kerbyfc/telegram-bridge/internal/server"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// stubAPI — порт Bot API, который в этих тестах не зовётся: проверяется транспорт, а не тема.
type stubAPI struct{}

// SendMessage не зовётся в тестах транспорта.
func (stubAPI) SendMessage(_ context.Context, _ int64, _ string) (int, error) { return 1, nil }

// EditMessageText не зовётся в тестах транспорта.
func (stubAPI) EditMessageText(_ context.Context, _ int, _ string) error { return nil }

// CreateTopic не зовётся в тестах транспорта.
func (stubAPI) CreateTopic(_ context.Context, _ string) (int64, error) { return 1, nil }

// RenameTopic не зовётся в тестах транспорта.
func (stubAPI) RenameTopic(_ context.Context, _ int64, _ string) error { return nil }

// CloseTopic не зовётся в тестах транспорта.
func (stubAPI) CloseTopic(_ context.Context, _ int64) error { return nil }

// ReopenTopic не зовётся в тестах транспорта.
func (stubAPI) ReopenTopic(_ context.Context, _ int64) error { return nil }

// SetTopicInMode не зовётся в тестах транспорта.
func (stubAPI) SetTopicInMode(_ context.Context, _ int64, _ bool) error { return nil }

// SetReaction не зовётся в тестах транспорта.
func (stubAPI) SetReaction(_ context.Context, _ int, _ botapi.Mark) error { return nil }

// PinMessage не зовётся в тестах транспорта.
func (stubAPI) PinMessage(_ context.Context, _ int64, _ int) error { return nil }

// SendChatAction не зовётся в тестах транспорта.
func (stubAPI) SendChatAction(_ context.Context, _ int64, _ string) error { return nil }

// GetUpdates в тестах транспорта ждёт отмены.
func (stubAPI) GetUpdates(ctx context.Context, _ int64, _ time.Duration) ([]botapi.Update, error) {
	<-ctx.Done()

	return nil, fmt.Errorf("опрос отменён: %w", ctx.Err())
}

// TestLongRequestDoesNotBlockShortOnes: ответ сопоставляется с запросом по `rid`, а не по
// порядку, поэтому долгий запрос на том же соединении не мешает коротким идти мимо него.
func TestLongRequestDoesNotBlockShortOnes(t *testing.T) {
	t.Parallel()

	socket, srv := listen(t)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	go func() { _ = srv.Serve(ctx) }()

	conn, err := dial(t, ctx, socket)
	if err != nil {
		t.Fatalf("соединение не открыто: %v", err)
	}

	defer func() { _ = conn.Close() }()

	reader := protocol.NewReader(conn)
	writer := protocol.NewWriter(conn)

	hello := protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpHello, RID: "r-hello",
		Role: protocol.RoleSession, Kind: string(protocol.KindMod), Session: "s1",
		Client: "claude-code",
	}
	send(t, writer, hello)
	read(t, reader, "r-hello")

	// Долгий запрос: службе нечего отдать, он висит.
	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpDeliveryWait, RID: "r-long",
	})

	// Короткий запрос идёт мимо него и отвечается первым.
	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpModeStatus, RID: "r-short",
	})

	resp := read(t, reader, "r-short")
	if !resp.OK {
		t.Fatalf("короткий вызов отказал: %+v", resp.Error)
	}
}

// TestStopDropsLongRequestWithoutAnswer: остановка службы рвёт соединение сессии, а долгому
// запросу не отвечает ничем: отказ на нём подкоманда `link` прочла бы концом канала, а обрыв —
// потерей связи, после которой звонят снова. Служба при этом выходит, не дожидаясь, пока
// соединение закроет сама сессия.
func TestStopDropsLongRequestWithoutAnswer(t *testing.T) {
	t.Parallel()

	socket, srv := listen(t)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	served := make(chan error, 1)

	go func() { served <- srv.Serve(ctx) }()

	conn, err := dial(t, context.Background(), socket)
	if err != nil {
		t.Fatalf("соединение не открыто: %v", err)
	}

	defer func() { _ = conn.Close() }()

	reader := protocol.NewReader(conn)
	writer := protocol.NewWriter(conn)

	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpHello, RID: "r-hello",
		Role: protocol.RoleSession, Kind: string(protocol.KindMod), Session: "s1",
		Client: "claude-code",
	})
	read(t, reader, "r-hello")

	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpDeliveryWait, RID: "r-long",
	})

	// Долгий запрос дошёл до ядра и висит: короткий на том же соединении уже отвечен.
	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpModeStatus, RID: "r-short",
	})
	read(t, reader, "r-short")

	stop()

	resp, err := reader.ReadResponse()
	if err == nil {
		t.Fatalf("оборванному долгому запросу пришёл ответ: %+v", resp)
	}

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("служба вышла с ошибкой: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("служба не вышла: ждёт, пока соединение закроет сессия")
	}
}

// TestMain отдаёт тестовый бинарь поддельной командой разворота, когда его зовут ею.
func TestMain(m *testing.M) {
	resolvetest.Serve()
	os.Exit(m.Run())
}

// slowAPI — порт Bot API, у которого отправка сообщения, когда тест её взвёл, висит, пока тест её
// не отпустит: так реплика агента застаёт остановку службы на середине.
type slowAPI struct {
	stubAPI

	armed   *atomic.Bool
	started chan struct{}
	release chan struct{}
}

// SendMessage висит, пока тест не отпустит отправку.
func (a slowAPI) SendMessage(ctx context.Context, _ int64, _ string) (int, error) {
	if !a.armed.Load() {
		return 1, nil
	}

	a.started <- struct{}{}

	select {
	case <-a.release:
		return 7, nil
	case <-ctx.Done():
		return 0, fmt.Errorf("отправка оборвана: %w", ctx.Err())
	}
}

// TestStopFinishesStartedReply: реплика агента, которая уже уходит в тему, остановкой службы не
// обрывается: служба дописывает её, отвечает на неё и только потом закрывает соединение. Новых
// запросов после остановки соединение не берёт.
func TestStopFinishesStartedReply(t *testing.T) {
	t.Parallel()

	api := slowAPI{
		stubAPI: stubAPI{}, armed: &atomic.Bool{},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}

	socket, srv := listenWith(t, api)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	served := make(chan error, 1)

	go func() { served <- srv.Serve(ctx) }()

	conn, err := dial(t, context.Background(), socket)
	if err != nil {
		t.Fatalf("соединение не открыто: %v", err)
	}

	defer func() { _ = conn.Close() }()

	reader := protocol.NewReader(conn)
	writer := protocol.NewWriter(conn)

	inMode(t, reader, writer)
	api.armed.Store(true)

	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpAgentMessage, RID: "r-reply", Text: "готово",
	})

	select {
	case <-api.started:
	case <-time.After(2 * time.Second):
		t.Fatal("реплика до Bot API не дошла")
	}

	stop()

	// Запрос после остановки соединение не берёт: ответа на него не будет.
	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpModeStatus, RID: "r-late",
	})

	close(api.release)

	resp, err := reader.ReadResponse()
	if err != nil || resp.RID != "r-reply" || !resp.OK {
		t.Fatalf("начатая реплика не дописана: %+v, %v", resp, err)
	}

	late, err := reader.ReadResponse()
	if err == nil {
		t.Fatalf("запрос после остановки получил ответ: %+v", late)
	}

	exited(t, served)
}

// exited ждёт выхода службы без ошибки.
func exited(t *testing.T, served chan error) {
	t.Helper()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("служба вышла с ошибкой: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("служба не вышла после остановки")
	}
}

// inMode представляет соединение сессией и включает ей режим: реплики агента идут только в режиме.
func inMode(t *testing.T, reader *protocol.Reader, writer *protocol.Writer) {
	t.Helper()

	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpHello, RID: "r-hello",
		Role: protocol.RoleSession, Kind: string(protocol.KindMod), Session: "s1",
		Client: "claude-code",
	})
	read(t, reader, "r-hello")

	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpModeOn, RID: "r-on", Name: "alpha",
	})

	on := read(t, reader, "r-on")
	if !on.OK {
		t.Fatalf("режим не включён: %+v", on.Error)
	}
}

// TestStopDropsHangingResolver: остановка снимает разворот ссылок в самом начале, а не после
// выхода: реплика, ждущая в очереди темы вывода висящей команды, уходит сразу, и служба кончается
// в срок автозапуска, а брошенная ссылка оставляет строку журнала `link-dropped`.
func TestStopDropsHangingResolver(t *testing.T) {
	t.Parallel()

	socket, srv, root := listenConfigured(t, stubAPI{}, func(cfg *config.Config) {
		res, err := config.NewLinkResolver(`(?m)^needs? input:(.*)$`, `x://[^\s)]+`,
			resolvetest.Command(resolvetest.ModeSleep, "{link}"), time.Minute)
		if err != nil {
			t.Fatalf("запись разворота не собрана: %v", err)
		}

		cfg.LinkResolvers = append(cfg.LinkResolvers, res)
	})

	stop, served, reader, writer := running(t, srv, socket)

	inMode(t, reader, writer)

	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpAgentMessage, RID: "r-link", Text: "needs input: x://a",
	})
	read(t, reader, "r-link")

	// Вторая реплика ждёт в очереди темы вывода висящей команды.
	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpAgentMessage, RID: "r-next", Text: "дальше",
	})

	// Короткий запрос следом отвечен — значит, вторая реплика уже взята соединением: кадры
	// читаются по порядку.
	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpModeStatus, RID: "r-status",
	})
	read(t, reader, "r-status")

	began := time.Now()

	stop()

	resp, err := reader.ReadResponse()
	if err != nil || resp.RID != "r-next" || !resp.OK {
		t.Fatalf("реплика за висящей командой не ушла: %+v, %v", resp, err)
	}

	exited(t, served)

	if spent := time.Since(began); spent > 3*time.Second {
		t.Fatalf("остановка шла %s: разворот снят не в её начале", spent)
	}

	srv.Close()

	journal, err := os.ReadFile(filepath.Join(root, "bridge.log"))
	if err != nil {
		t.Fatalf("журнал не прочитан: %v", err)
	}

	if !strings.Contains(string(journal), "link-dropped") {
		t.Fatalf("брошенная ссылка не оставила строки `link-dropped`:\n%s", journal)
	}
}

// running запускает службу и открывает к ней соединение: отдаёт остановку, исход службы и концы
// соединения.
func running(
	t *testing.T, srv *server.Server, socket string,
) (context.CancelFunc, chan error, *protocol.Reader, *protocol.Writer) {
	t.Helper()

	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)

	served := make(chan error, 1)

	go func() { served <- srv.Serve(ctx) }()

	conn, err := dial(t, context.Background(), socket)
	if err != nil {
		t.Fatalf("соединение не открыто: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return stop, served, protocol.NewReader(conn), protocol.NewWriter(conn)
}

// TestStopDropsReplyPastGrace: реплика, которую служба не дописала за срок дописывания, ответа не
// получает: отказ «вызов прерван» подкоманда прочла бы концом реплики, а обрыв — потерей связи, и
// она отдаст реплику вернувшейся службе.
func TestStopDropsReplyPastGrace(t *testing.T) {
	t.Parallel()

	api := slowAPI{
		stubAPI: stubAPI{}, armed: &atomic.Bool{},
		started: make(chan struct{}, 1), release: make(chan struct{}),
	}

	socket, srv := listenWith(t, api)
	srv.SetGrace(100 * time.Millisecond)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	served := make(chan error, 1)

	go func() { served <- srv.Serve(ctx) }()

	conn, err := dial(t, context.Background(), socket)
	if err != nil {
		t.Fatalf("соединение не открыто: %v", err)
	}

	defer func() { _ = conn.Close() }()

	reader := protocol.NewReader(conn)
	writer := protocol.NewWriter(conn)

	inMode(t, reader, writer)
	api.armed.Store(true)

	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpAgentMessage, RID: "r-reply", Text: "готово",
	})

	<-api.started

	stop()

	resp, err := reader.ReadResponse()
	if err == nil {
		t.Fatalf("реплика, оборванная сроком дописывания, получила ответ: %+v", resp)
	}

	exited(t, served)
}

// TestSocketPermissions: сокет закрыт правами на владельца — единственная настоящая граница
// доступа здесь пользователь системы.
func TestSocketPermissions(t *testing.T) {
	t.Parallel()

	socket, srv := listen(t)

	defer srv.Close()

	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("сокета нет: %v", err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("права сокета: %v", info.Mode().Perm())
	}
}

// TestFirstFrameMustBeHello: первый кадр соединения — `hello`, молчаливого разбора «по тем
// полям, что понял» протокол не знает.
func TestFirstFrameMustBeHello(t *testing.T) {
	t.Parallel()

	socket, srv := listen(t)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	go func() { _ = srv.Serve(ctx) }()

	conn, err := dial(t, ctx, socket)
	if err != nil {
		t.Fatalf("соединение не открыто: %v", err)
	}

	defer func() { _ = conn.Close() }()

	writer := protocol.NewWriter(conn)
	send(t, writer, protocol.Request{ //nolint:exhaustruct // кадр заполняется по операции
		V: protocol.Version, Op: protocol.OpModeStatus, RID: "r-1",
	})

	resp := read(t, protocol.NewReader(conn), "r-1")
	if resp.OK || resp.Error.Code != protocol.CodeBadRequest {
		t.Fatalf("кадр до `hello` принят: %+v", resp)
	}
}

// listen поднимает службу на своём сокете.
func listen(t *testing.T) (string, *server.Server) {
	t.Helper()

	return listenWith(t, stubAPI{})
}

// listenWith поднимает службу на своём сокете с названным портом Bot API.
func listenWith(t *testing.T, api botapi.API) (string, *server.Server) {
	t.Helper()

	socket, srv, _ := listenConfigured(t, api, func(*config.Config) {})

	return socket, srv
}

// listenConfigured поднимает службу с правкой настроек и отдаёт ещё и корень состояния: в нём
// журнал службы.
func listenConfigured(
	t *testing.T, api botapi.API, adjust func(*config.Config),
) (string, *server.Server, string) {
	t.Helper()

	root := t.TempDir()

	// Путь сокета берётся коротким: предел длины адреса Unix-сокета меньше, чем длина
	// временного каталога теста на macOS.
	//nolint:usetesting // t.TempDir даёт путь длиннее предела адреса Unix-сокета на macOS
	sockDir, err := os.MkdirTemp("/tmp", "tgb")
	if err != nil {
		t.Fatalf("каталог сокета не создан: %v", err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	socket := filepath.Join(sockDir, "bridge.sock")
	cfg := &config.Config{ //nolint:exhaustruct // транспорту хватает этих ключей
		BotToken: "токен", GroupID: -100, OwnerID: 1, StateRoot: root,
		DefaultTopicName: "alpha", SocketPath: socket, PollTimeout: time.Second,
		ReconnectGrace: time.Minute, ContextEditMin: time.Second,
		// Корень вклада в статусную строку — свой на тест. Без ключа он разрешается общим с
		// читателем правилом, то есть указывает в настоящее состояние строки у владельца, и
		// тест писал бы вклад туда.
		StatuslineRoot: root,
	}

	st, err := store.Open(root)
	if err != nil {
		t.Fatalf("состояние не открыто: %v", err)
	}

	jrnl, err := store.OpenJournal(
		filepath.Join(root, "bridge.log"), config.DefaultLogMaxBytes, config.DefaultLogKeep,
	)
	if err != nil {
		t.Fatalf("журнал не открыт: %v", err)
	}

	t.Cleanup(func() { _ = jrnl.Close() })

	adjust(cfg)

	engine, err := core.New(cfg, api, st, jrnl)
	if err != nil {
		t.Fatalf("ядро не поднялось: %v", err)
	}

	srv, err := server.Listen(context.Background(), socket, engine)
	if err != nil {
		t.Fatalf("сокет не поднят: %v", err)
	}

	return socket, srv, root
}

// dial открывает соединение со службой.
func dial(t *testing.T, ctx context.Context, socket string) (net.Conn, error) {
	t.Helper()

	dialer := net.Dialer{} //nolint:exhaustruct // соединению хватает значений по умолчанию

	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("соединение не открыто: %w", err)
	}

	return conn, nil
}

// send отправляет кадр запроса.
func send(t *testing.T, writer *protocol.Writer, req protocol.Request) {
	t.Helper()

	err := writer.WriteRequest(req)
	if err != nil {
		t.Fatalf("кадр не отправлен: %v", err)
	}
}

// read читает кадры, пока не придёт ответ с нужным `rid`.
func read(t *testing.T, reader *protocol.Reader, rid string) protocol.Response {
	t.Helper()

	for range 8 {
		resp, err := reader.ReadResponse()
		if err != nil {
			t.Fatalf("ответ не прочитан: %v", err)
		}

		if resp.RID == rid {
			return resp
		}
	}

	t.Fatalf("ответа на %q не пришло", rid)

	return protocol.Response{} //nolint:exhaustruct // недостижимо после t.Fatalf
}
