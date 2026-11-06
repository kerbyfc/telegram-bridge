// Package server — транспорт службы: адрес границы (Unix-сокет или именованный канал), кадр на
// строку, по соединению своя горутина. Долгий запрос на соединении не мешает коротким идти мимо
// него, поэтому каждый запрос исполняется своей горутиной, а ответы сопоставляются по `rid`.
package server

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/core"
	"github.com/kerbyfc/telegram-bridge/internal/endpoint"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// Server слушает адрес службы.
type Server struct {
	core     *core.Core
	listener net.Listener
	conns    sync.WaitGroup
}

// drainGrace — сколько остановка ждёт начатых коротких запросов: реплика агента, которая уже
// уходит в тему, дописывается в пределах одного вызова Bot API, а не обрывается на середине.
// Меньше срока, который даёт остановке launchd (20 с), чтобы служба вышла сама.
const drainGrace = 15 * time.Second

// Addr отдаёт адрес, на котором служба слушает.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Serve принимает соединения, пока не кончится ctx.
func (s *Server) Serve(ctx context.Context) error {
	drain, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()

	go func() {
		select {
		case <-ctx.Done():
		case <-drain.Done():
			return
		}

		timer := time.NewTimer(drainGrace)
		defer timer.Stop()

		select {
		case <-timer.C:
			cancel()
		case <-drain.Done():
		}
	}()

	s.core.Ready()

	go func() {
		<-ctx.Done()

		_ = s.listener.Close()
	}()

	err := s.accept(ctx, drain)

	s.conns.Wait()

	return err
}

// accept принимает соединения до конца ctx; отказ приёма при живом ctx — громкая ошибка.
func (s *Server) accept(ctx context.Context, drain context.Context) error {
	for ctx.Err() == nil {
		conn, err := s.listener.Accept()
		if err != nil {
			return acceptFailed(ctx, err)
		}

		s.conns.Add(1)

		go s.handleConn(ctx, drain, conn)
	}

	return nil
}

// Close закрывает адрес и ждёт соединений.
func (s *Server) Close() {
	_ = s.listener.Close()
	s.conns.Wait()
}

// handleConn ведёт одно соединение: читает кадры и отвечает на каждый.
//
// Остановка службы рвёт соединение, а не отвечает на него: долгий запрос, оборванный остановкой,
// ответа не получает вовсе, и подкоманда `link` видит потерю связи, после которой звонят снова, а
// не отказ, после которого не звонят. Короткий запрос, начатый до остановки, остановка не обрывает:
// реплика агента, которая уже уходит в тему, дописывается и получает ответ — в пределах срока
// дописывания `drainGrace`, — и только потом соединение закрывается. Новых запросов после
// остановки соединение не берёт: они остаются без ответа, и подкоманда повторит их новой службе.
//
// drain — срок дописывания коротких запросов: остановка службы его не кончает, а кончает
// `drainGrace` после неё.
func (s *Server) handleConn(ctx context.Context, drain context.Context, conn net.Conn) {
	defer s.conns.Done()

	// waitCtx — долгий запрос: его кончают и остановка службы, и конец соединения.
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()

	// shortCtx — короткий запрос: его кончает конец соединения и срок дописывания, а не остановка.
	shortCtx, cancelShort := context.WithCancel(drain)
	defer cancelShort()

	handle := s.core.NewConn()

	// Закрытие соединения переживает отмену его ctx: служба на нём говорит владельцу, что его
	// слово осталось в ящике, и ctx закрывшегося соединения этой отправке не указ.
	defer s.core.CloseConn(context.WithoutCancel(ctx), handle)

	reader := protocol.NewReader(conn)
	link := newWire(conn)

	go func() {
		<-waitCtx.Done()

		link.drain()
	}()

	var requests sync.WaitGroup

	for {
		req, err := reader.Read()
		if err != nil {
			break
		}

		long := req.Op == protocol.OpDeliveryWait

		if ctx.Err() != nil || (!long && !link.take()) {
			continue
		}

		requests.Add(1)

		go func() {
			defer requests.Done()

			if long {
				s.answer(waitCtx, link, handle, req, true)

				return
			}

			defer link.short.Done()

			s.answer(shortCtx, link, handle, req, false)
		}()
	}

	cancelWait()
	cancelShort()
	link.shut()

	requests.Wait()
}

// answer исполняет запрос и отдаёт ответ. Ответ долгого запроса, собранный после конца
// соединения или остановки службы, не уходит: на остановке это ответ оборванному запросу, а не его
// исход.
func (s *Server) answer(ctx context.Context, link *wire, handle *core.Conn, req protocol.Request, long bool) {
	resp := s.core.Handle(ctx, handle, req)

	if long {
		link.write(resp, ctx.Done())

		return
	}

	link.write(resp, nil)
}

// wire — запись в одно соединение и его закрытие на остановке службы.
type wire struct {
	conn   net.Conn
	writer *protocol.Writer

	mu       sync.Mutex
	closed   bool
	stopping bool
	short    sync.WaitGroup
}

// take отмечает начало короткого запроса; после остановки новых не берёт.
func (w *wire) take() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.stopping {
		return false
	}

	w.short.Add(1)

	return true
}

// drain дожидается начатых коротких запросов и закрывает соединение.
func (w *wire) drain() {
	w.mu.Lock()
	w.stopping = true
	w.mu.Unlock()

	w.short.Wait()
	w.shut()
}

// write отдаёт ответ, пока соединение открыто; ответ долгого запроса — только пока не закрыт
// `gone`, конец его ctx. Пустой `gone` не закрывается никогда.
func (w *wire) write(resp protocol.Response, gone <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return
	}

	select {
	case <-gone:
		return
	default:
	}

	_ = w.writer.Write(resp)
}

// shut закрывает соединение.
func (w *wire) shut() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.closed = true
	_ = w.conn.Close()
}

// Listen поднимает адрес службы по названному адресу. Чем он поднят — сокетом или каналом, —
// решает площадка: права, каталог и остатки прежнего запуска держит internal/endpoint.
func Listen(ctx context.Context, address string, engine *core.Core) (*Server, error) {
	listener, err := endpoint.Listen(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("%s службы не поднят: %w", endpoint.Word(), err)
	}

	return &Server{core: engine, listener: listener, conns: sync.WaitGroup{}}, nil
}

// acceptFailed отличает закрытие адреса остановкой службы от настоящего отказа приёма.
func acceptFailed(ctx context.Context, err error) error {
	select {
	case <-ctx.Done():
		return nil
	default:
		return fmt.Errorf("соединение не принято: %w", err)
	}
}

// newWire собирает запись в соединение.
func newWire(conn net.Conn) *wire {
	return &wire{
		conn: conn, writer: protocol.NewWriter(conn),
		mu: sync.Mutex{}, closed: false, stopping: false, short: sync.WaitGroup{},
	}
}
