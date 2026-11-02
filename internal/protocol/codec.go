package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxFrameBytes — предел длины кадра. Кадр длиннее — негодный вызов, а не повод тянуть память.
const MaxFrameBytes = 1 << 20

// ErrFrameTooLong — кадр длиннее предела.
var ErrFrameTooLong = errors.New("кадр длиннее предела")

// Reader читает кадры запросов построчно.
type Reader struct {
	src *bufio.Reader
}

// Writer пишет кадры ответов построчно; запись под мьютексом, потому что на одном соединении
// долгий ответ и короткие идут вразнобой.
type Writer struct {
	mu  sync.Mutex
	dst io.Writer
}

// Read читает один кадр запроса. Пустых строк в протоколе нет, поэтому они пропускаются как
// мусор переноса, а не разбираются.
func (r *Reader) Read() (Request, error) {
	var req Request

	err := r.frame(&req)
	if err != nil {
		return Request{}, err
	}

	return req, nil
}

// ReadResponse читает один кадр ответа: вторая сторона того же протокола.
func (r *Reader) ReadResponse() (Response, error) {
	var resp Response

	err := r.frame(&resp)
	if err != nil {
		return Response{}, err
	}

	return resp, nil
}

// frame читает строку и разбирает её в кадр.
func (r *Reader) frame(into any) error {
	for {
		line, err := r.line()
		if err != nil {
			return err
		}

		if len(line) == 0 {
			continue
		}

		err = json.Unmarshal(line, into)
		if err != nil {
			return fmt.Errorf("кадр не разобран: %w", err)
		}

		return nil
	}
}

// line читает одну строку без перевода, отбивая слишком длинные кадры.
func (r *Reader) line() ([]byte, error) {
	var full []byte

	for {
		chunk, more, err := r.src.ReadLine()
		if err != nil {
			return nil, err //nolint:wrapcheck // io.EOF различается звонящим по значению
		}

		full = append(full, chunk...)
		if len(full) > MaxFrameBytes {
			return nil, ErrFrameTooLong
		}

		if !more {
			return full, nil
		}
	}
}

// Write отправляет кадр ответа одной строкой.
func (w *Writer) Write(resp Response) error {
	return w.frame(resp)
}

// WriteRequest отправляет кадр запроса одной строкой: вторая сторона того же протокола.
func (w *Writer) WriteRequest(req Request) error {
	return w.frame(req)
}

// frame собирает кадр и отправляет его одной строкой.
func (w *Writer) frame(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("кадр не собран: %w", err)
	}

	body = append(body, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()

	_, err = w.dst.Write(body)
	if err != nil {
		return fmt.Errorf("кадр не отправлен: %w", err)
	}

	return nil
}

// NewReader готовит читателя кадров.
func NewReader(src io.Reader) *Reader {
	return &Reader{src: bufio.NewReaderSize(src, 64<<10)}
}

// NewWriter готовит писателя кадров.
func NewWriter(dst io.Writer) *Writer {
	return &Writer{mu: sync.Mutex{}, dst: dst}
}
