// Package link — соединение сессии со службой, которое держит средство сессии. Мод Claude Code
// живёт внутри процесса клиента, сокетов у него нет вовсе, поэтому соединение сессии держит эта
// подкоманда: она открывает его один раз, ведёт единственный долгий запрос `delivery.wait` и
// отдаёт моду доставку строкой своей стандартной выдачи, а подтверждение приёма принимает
// коротким запросом на свой местный вход.
package link

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/kerbyfc/telegram-bridge/internal/endpoint"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// Client — соединение со службой: кадр на строку, ответ сопоставляется с запросом по `rid`,
// поэтому долгий `delivery.wait` не мешает короткому подтверждению идти мимо него.
type Client struct {
	conn   net.Conn
	reader *protocol.Reader
	writer *protocol.Writer

	mu    sync.Mutex
	seq   uint64
	waits map[string]chan protocol.Response

	done chan struct{}
	once sync.Once
}

// Call отправляет запрос и ждёт ответа с тем же `rid`. Срока у вызова нет: его задаёт звонящий
// своим ctx, а `delivery.wait` ждёт, пока службе нечего отдать.
func (c *Client) Call(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	req.V = protocol.Version
	req.RID = c.rid()
	answer := c.expect(req.RID)

	defer c.forget(req.RID)

	err := c.writer.WriteRequest(req)
	if err != nil {
		return protocol.Response{}, fmt.Errorf("запрос `%s` не отправлен: %w", req.Op, err)
	}

	select {
	case <-ctx.Done():
		return protocol.Response{}, fmt.Errorf("запрос `%s` прерван: %w", req.Op, ctx.Err())
	case <-c.done:
		return protocol.Response{}, fmt.Errorf("%w: запрос `%s` остался без ответа", ErrLinkLost, req.Op)
	case resp := <-answer:
		return resp, nil
	}
}

// Done закрывается, когда соединение кончилось: чтение кадров встало.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// Close закрывает соединение.
func (c *Client) Close() {
	_ = c.conn.Close()
}

// read читает ответы, пока соединение живо, и разводит их по ждущим. Конец чтения — конец
// соединения: ждущие узнают об этом по закрытому `done`.
func (c *Client) read() {
	defer c.finish()

	for {
		resp, err := c.reader.ReadResponse()
		if err != nil {
			return
		}

		c.deliver(resp)
	}
}

// deliver отдаёт ответ тому, кто его ждёт. Ответ без ждущего — кадр на запрос, который уже
// кончился: он никому не нужен и молча пропускается.
func (c *Client) deliver(resp protocol.Response) {
	c.mu.Lock()
	answer, waited := c.waits[resp.RID]
	delete(c.waits, resp.RID)
	c.mu.Unlock()

	if !waited {
		return
	}

	answer <- resp
}

// expect заводит ждущего ответа на запрос.
func (c *Client) expect(rid string) chan protocol.Response {
	answer := make(chan protocol.Response, 1)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.waits[rid] = answer

	return answer
}

// forget убирает ждущего, чей запрос кончился.
func (c *Client) forget(rid string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.waits, rid)
}

// rid выдаёт следующий id запроса этого соединения.
func (c *Client) rid() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seq++

	return "l-" + strconv.FormatUint(c.seq, 10)
}

// finish отмечает конец соединения.
func (c *Client) finish() {
	c.once.Do(func() { close(c.done) })
}

// Dial открывает соединение со службой. Приветствие — дело звонящего: первый кадр соединения
// называет роль, вид звонящего и сессию.
func Dial(ctx context.Context, address string) (*Client, error) {
	conn, err := endpoint.Dial(ctx, address)
	if err != nil {
		return nil, err //nolint:wrapcheck // причина названа адресом и площадкой внутри endpoint
	}

	return newClient(conn), nil
}

// newClient заводит соединение со службой поверх открытого соединения и читает его ответы.
func newClient(conn net.Conn) *Client {
	client := &Client{
		conn:   conn,
		reader: protocol.NewReader(conn),
		writer: protocol.NewWriter(conn),
		mu:     sync.Mutex{},
		seq:    0,
		waits:  make(map[string]chan protocol.Response),
		done:   make(chan struct{}),
		once:   sync.Once{},
	}

	go client.read()

	return client
}
