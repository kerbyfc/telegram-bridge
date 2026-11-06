// Package server — транспорт службы: адрес границы (Unix-сокет или именованный канал), кадр на
// строку, по соединению своя горутина. Долгий запрос на соединении не мешает коротким идти мимо
// него, поэтому каждый запрос исполняется своей горутиной, а ответы сопоставляются по `rid`.
package server

import (
	"context"
	"fmt"
	"net"
	"sync"

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

// Addr отдаёт адрес, на котором служба слушает.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Serve принимает соединения, пока не кончится ctx.
func (s *Server) Serve(ctx context.Context) error {
	s.core.Ready()

	go func() {
		<-ctx.Done()

		_ = s.listener.Close()
	}()

	err := s.accept(ctx)

	s.conns.Wait()

	return err
}

// accept принимает соединения до конца ctx; отказ приёма при живом ctx — громкая ошибка.
func (s *Server) accept(ctx context.Context) error {
	for ctx.Err() == nil {
		conn, err := s.listener.Accept()
		if err != nil {
			return acceptFailed(ctx, err)
		}

		s.conns.Add(1)

		go s.handleConn(ctx, conn)
	}

	return nil
}

// Close закрывает адрес и ждёт соединений.
func (s *Server) Close() {
	_ = s.listener.Close()
	s.conns.Wait()
}

// handleConn ведёт одно соединение: читает кадры и отвечает на каждый.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer s.conns.Done()

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	handle := s.core.NewConn()

	// Закрытие соединения переживает отмену его ctx: служба на нём говорит владельцу, что его
	// слово осталось в ящике, и ctx закрывшегося соединения этой отправке не указ.
	defer s.core.CloseConn(context.WithoutCancel(ctx), handle)

	reader := protocol.NewReader(conn)
	writer := protocol.NewWriter(conn)

	// Остановка службы рвёт соединение, а не отвечает на него: долгий запрос, оборванный
	// остановкой, ответа не получает вовсе, и подкоманда `link` видит потерю связи, после которой
	// звонят снова, а не отказ, после которого не звонят.
	var out sync.Mutex

	go func() {
		<-connCtx.Done()

		out.Lock()
		defer out.Unlock()

		_ = conn.Close()
	}()

	var requests sync.WaitGroup

	for {
		req, err := reader.Read()
		if err != nil {
			break
		}

		requests.Add(1)

		go func() {
			defer requests.Done()

			resp := s.core.Handle(connCtx, handle, req)

			out.Lock()
			defer out.Unlock()

			// Ответ, собранный после конца соединения, не уходит: его ctx кончился раньше, чем
			// ответ готов, и на остановке это ответ оборванному запросу, а не его исход.
			if connCtx.Err() != nil {
				return
			}

			_ = writer.Write(resp)
		}()
	}

	cancel()

	_ = conn.Close()

	requests.Wait()
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
