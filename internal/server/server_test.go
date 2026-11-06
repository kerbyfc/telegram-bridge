package server_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/core"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
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

	engine, err := core.New(cfg, stubAPI{}, st, jrnl)
	if err != nil {
		t.Fatalf("ядро не поднялось: %v", err)
	}

	srv, err := server.Listen(context.Background(), socket, engine)
	if err != nil {
		t.Fatalf("сокет не поднят: %v", err)
	}

	return socket, srv
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
