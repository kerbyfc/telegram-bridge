package link_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/link"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// patience — сколько тест ждёт кадра, события или ответа. Срок щедрый: ждут соединения в памяти,
// и его истечение значит, что сторона не сделала того, что обязана.
const patience = 2 * time.Second

// retry — пауза перед новым соединением в тестах: возврат проверяется, а не выдерживается.
const retry = 10 * time.Millisecond

// stopFarewell — срок `bye` на остановке в тестах: на него тест и смотрит, когда служба молчит.
const stopFarewell = 100 * time.Millisecond

// quiet — срок тишины: за него сторона обязана не послать ни одного кадра. Один на все проверки
// тишины, потому что разной его делать нечему.
const quiet = 50 * time.Millisecond

// service — служба в тесте: слушает свой сокет, кладёт кадры в канал и отвечает тем, что положит
// тест. Ядра здесь нет вовсе — проверяется сторона Claude Code.
type service struct {
	path     string
	requests chan protocol.Request

	mu       sync.Mutex
	listener net.Listener
	writer   *protocol.Writer
	conn     net.Conn
	links    int
}

// probe — запущенное соединение сессии: события его стандартной выдачи и ответ его Run.
type probe struct {
	link   *link.Link
	events chan link.Event
	done   chan error
	stop   context.CancelFunc
}

// Endpoint отдаёт адрес службы в тесте.
func (s *service) Endpoint() string {
	return s.path
}

// vanish гасит службу целиком, как остановка: адрес закрыт, сокета нет, соединение оборвано без
// ответа.
func (s *service) vanish(t *testing.T) {
	t.Helper()

	s.mu.Lock()
	listener, conn := s.listener, s.conn
	s.conn = nil
	s.writer = nil
	s.mu.Unlock()

	_ = listener.Close()

	if conn != nil {
		_ = conn.Close()
	}

	_ = os.Remove(s.path)
}

// revive поднимает службу снова на том же адресе: так кончается её перезапуск.
func (s *service) revive(t *testing.T) {
	t.Helper()

	s.serve(t)
}

// serve слушает адрес службы и принимает соединения.
func (s *service) serve(t *testing.T) {
	t.Helper()

	cfg := net.ListenConfig{} //nolint:exhaustruct // слушателю хватает значений по умолчанию

	listener, err := cfg.Listen(context.Background(), "unix", s.path)
	if err != nil {
		t.Fatalf("сокет службы не поднят: %v", err)
	}

	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()

	t.Cleanup(func() { _ = listener.Close() })

	go s.accept(listener)
}

// take ждёт следующий кадр, который увидела служба.
func (s *service) take(t *testing.T) protocol.Request {
	t.Helper()

	select {
	case req := <-s.requests:
		return req
	case <-time.After(patience):
		t.Fatal("служба кадра не дождалась")

		return protocol.Request{} //nolint:exhaustruct // до возврата дело не доходит: тест уже упал
	}
}

// silent проверяет, что за срок тишины службе не пришло ни одного кадра.
func (s *service) silent(t *testing.T) {
	t.Helper()

	select {
	case req := <-s.requests:
		t.Fatalf("службе пришёл кадр `%s`, которого тут быть не должно", req.Op)
	case <-time.After(quiet):
	}
}

// reply отвечает по текущему соединению.
func (s *service) reply(t *testing.T, resp protocol.Response) {
	t.Helper()

	s.mu.Lock()
	writer := s.writer
	s.mu.Unlock()

	if writer == nil {
		t.Fatal("ответить некуда: соединения нет")
	}

	err := writer.Write(resp)
	if err != nil {
		t.Fatalf("ответ не отправлен: %v", err)
	}
}

// ok отвечает удачей на запрос.
func (s *service) ok(t *testing.T, req protocol.Request) {
	t.Helper()
	s.reply(t, protocol.Ok(req.RID))
}

// fail отвечает отказом с кодом контракта.
func (s *service) fail(t *testing.T, req protocol.Request, code protocol.Code, text string) {
	t.Helper()
	s.reply(t, protocol.Fail(req.RID, code, text))
}

// hand отдаёт долгим ответом слово владельца.
func (s *service) hand(t *testing.T, req protocol.Request, delivery string, text string) {
	t.Helper()

	resp := protocol.Ok(req.RID)
	resp.Kind = protocol.DeliveryOwnerMessage
	resp.DeliveryID = delivery
	resp.Text = text
	resp.Provenance = "Доставлено из Telegram, автор Александр, человек"
	resp.Author = &protocol.Author{ID: 11111111, Name: "Александр", Kind: protocol.AuthorHuman}
	resp.Topic = &protocol.TopicRef{ID: 42, Name: "tg-inventory"}

	s.reply(t, resp)
}

// command отдаёт долгим ответом команду из темы.
func (s *service) command(t *testing.T, req protocol.Request, delivery string, text string) {
	t.Helper()

	resp := protocol.Ok(req.RID)
	resp.Kind = protocol.DeliveryClientCmd
	resp.DeliveryID = delivery
	resp.Text = text
	resp.Provenance = "Доставлено из Telegram, автор Александр, человек"
	resp.Author = &protocol.Author{ID: 11111111, Name: "Александр", Kind: protocol.AuthorHuman}

	s.reply(t, resp)
}

// drop рвёт текущее соединение: так служба уходит на перезапуск.
func (s *service) drop(t *testing.T) {
	t.Helper()

	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		t.Fatal("рвать нечего: соединения нет")
	}

	_ = conn.Close()
}

// opened отвечает, сколько соединений служба приняла.
func (s *service) opened() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.links
}

// accept принимает соединения, пока слушатель жив.
func (s *service) accept(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		s.mu.Lock()
		s.conn = conn
		s.writer = protocol.NewWriter(conn)
		s.links++
		s.mu.Unlock()

		go s.read(conn)
	}
}

// read читает кадры одного соединения.
func (s *service) read(conn net.Conn) {
	reader := protocol.NewReader(conn)

	for {
		req, err := reader.Read()
		if err != nil {
			return
		}

		s.requests <- req
	}
}

// event ждёт следующее событие стандартной выдачи.
func (p *probe) event(t *testing.T) link.Event {
	t.Helper()

	select {
	case event := <-p.events:
		return event
	case <-time.After(patience):
		t.Fatal("события от соединения сессии не дождались")

		return link.Event{} //nolint:exhaustruct // до возврата дело не доходит: тест уже упал
	}
}

// ack посылает подтверждение приёма местным входом, как это делает мод.
func (p *probe) ack(t *testing.T, ack link.Ack) (int, link.Answer) {
	t.Helper()

	return p.post(t, link.PathAck, ack)
}

// bye говорит местному входу, что сессия кончилась.
func (p *probe) bye(t *testing.T) (int, link.Answer) {
	t.Helper()

	return p.post(t, link.PathBye, struct{}{})
}

// post отправляет короткий запрос на местный вход соединения сессии.
func (p *probe) post(t *testing.T, path string, body any) (int, link.Answer) {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("запрос не собран: %v", err)
	}

	socket := p.link.Front()
	client := http.Client{ //nolint:exhaustruct // клиенту местного входа хватает своего транспорта
		Transport: &http.Transport{ //nolint:exhaustruct // транспорту хватает своего набора
			DialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
				dialer := net.Dialer{} //nolint:exhaustruct // соединению хватает значений по умолчанию

				return dialer.DialContext(ctx, "unix", socket)
			},
		},
		Timeout: patience,
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://telegram-bridge"+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("запрос местного входа не собран: %v", err)
	}

	request.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(request)
	if err != nil {
		t.Fatalf("местный вход не ответил: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	var answer link.Answer

	err = json.NewDecoder(resp.Body).Decode(&answer)
	if err != nil {
		t.Fatalf("ответ местного входа не разобран: %v", err)
	}

	return resp.StatusCode, answer
}

// ended ждёт конца Run и отдаёт его ошибку.
func (p *probe) ended(t *testing.T) error {
	t.Helper()

	select {
	case err := <-p.done:
		return err
	case <-time.After(patience):
		t.Fatal("соединение сессии не кончилось")

		return nil
	}
}

// listen поднимает службу в тесте: сокет в своём каталоге теста.
func listen(t *testing.T) *service {
	t.Helper()

	// Каталог сокета короткий: предел длины адреса Unix-сокета каталог теста не выдерживает —
	// в его имени стоит имя теста, и адрес перестаёт влезать в предел.
	//
	//nolint:usetesting // t.TempDir даёт слишком длинный путь для адреса Unix-сокета
	dir, err := os.MkdirTemp("", "dmtg-test-")
	if err != nil {
		t.Fatalf("каталог сокета службы не создан: %v", err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	srv := &service{
		path:     filepath.Join(dir, "b.sock"),
		requests: make(chan protocol.Request, 16),
		mu:       sync.Mutex{},
		listener: nil,
		writer:   nil,
		conn:     nil,
		links:    0,
	}

	srv.serve(t)

	return srv
}

// start запускает соединение сессии мода Claude Code против службы теста и читает его события.
func start(t *testing.T, srv *service) *probe {
	t.Helper()

	return startAs(t, srv, protocol.ClientClaudeCode, protocol.KindMod)
}

// startAs запускает соединение сессии названного клиента: подкоманда у мода и у плагина-адаптера
// одна, и тесты обоих клиентов идут одним стендом.
func startAs(t *testing.T, srv *service, client protocol.Client, kind protocol.Kind) *probe {
	t.Helper()

	return startWith(t, srv, client, kind, time.Hour)
}

// startWith запускает соединение сессии со своим сроком молчаливой пропажи службы.
func startWith(
	t *testing.T, srv *service, client protocol.Client, kind protocol.Kind, absent time.Duration,
) *probe {
	t.Helper()

	reader, writer := io.Pipe()
	events := make(chan link.Event, 16)

	go scan(reader, events)

	session := link.New(link.Options{
		Endpoint:     srv.Endpoint(),
		Session:      session,
		Client:       client,
		Kind:         kind,
		Retry:        retry,
		Absent:       absent,
		StopFarewell: stopFarewell,
		Events:       writer,
		Log:          io.Discard,
	})

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- session.Run(ctx) }()

	t.Cleanup(func() {
		stop()

		_ = writer.Close()
	})

	return &probe{link: session, events: events, done: done, stop: stop}
}

// scan разбирает строки стандартной выдачи в события.
func scan(reader io.Reader, events chan link.Event) {
	lines := bufio.NewScanner(reader)

	for lines.Scan() {
		var event link.Event

		err := json.Unmarshal(lines.Bytes(), &event)
		if err != nil {
			continue
		}

		events <- event
	}
}

// still проверяет, что за срок подкоманда не отдала ни одного события.
func (p *probe) still(t *testing.T, span time.Duration) {
	t.Helper()

	select {
	case event := <-p.events:
		t.Fatalf("событие `%s` там, где их быть не должно: %+v", event.Event, event)
	case <-time.After(span):
	}
}

// running проверяет, что подкоманда не кончилась.
func (p *probe) running(t *testing.T) {
	t.Helper()

	select {
	case err := <-p.done:
		t.Fatalf("подкоманда кончилась, хотя связь вернулась: %v", err)
	case <-time.After(quiet):
	}
}
