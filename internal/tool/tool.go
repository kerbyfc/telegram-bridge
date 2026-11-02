// Package tool — тонкий клиент сессии: короткий вызов службы вместо запуска моста на каждый чих.
// Процесс живёт секунду, знает адрес службы и свою сессию — и больше ничего: ни путей внутри
// состояния, ни устройства моста, ни интерпретатора.
//
// Один вызов — одно соединение: приветствие ролью `tool`, один запрос, закрытие. Живости такое
// соединение не несёт, режима не снимает и ход сессии не держит: ждать ему нечего, кроме ответа
// на свой же запрос.
package tool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/link"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// DefaultTimeout — срок короткого вызова. Он покрывает вызов Bot API с повторами: включение
// режима заводит тему, а `say` отправляет сообщение, и оба идут через сеть. Больше этого ждать
// нечего — служба отвечает кодом, а не молчанием.
const DefaultTimeout = 90 * time.Second

// DialTimeout — срок на соединение со службой. Отдельный и короткий: не отвечающий адрес значит,
// что службы нет, и выяснять это минуту незачем.
const DialTimeout = 2 * time.Second

// ErrNoService — службы нет: адреса нет или за ним никто не слушает. Новое соединение делу не
// поможет — слушать некому.
var ErrNoService = errors.New("службы моста нет")

// ErrSilent — служба есть, но за срок не ответила: соединение состоялось, а кадра ответа нет.
// Для звонящего это не «службы нет»: живая служба держит состояние в памяти и на молчании, и
// принимать её за снятую значит писать поверх неё.
var ErrSilent = errors.New("служба моста не ответила за срок")

// ErrRefused — служба отказала кодом контракта.
var ErrRefused = errors.New("служба моста отказала")

// Options — чем описан короткий вызов.
type Options struct {
	// Endpoint — адрес службы: путь сокета на macOS, имя канала на Windows.
	Endpoint string
	// Session — сессия, о которой идёт речь; пусто — вызов не о сессии (самопроверка).
	Session string
	// Timeout — срок вызова; нуль — DefaultTimeout.
	Timeout time.Duration
}

// Call делает один короткий вызов и отдаёт ответ службы. Отказ службы — ошибка с кодом
// контракта: тихого успеха на отказе у протокола нет.
func Call(ctx context.Context, opts Options, req protocol.Request) (protocol.Response, error) {
	limit := opts.Timeout
	if limit == 0 {
		limit = DefaultTimeout
	}

	callCtx, done := context.WithTimeout(ctx, limit)
	defer done()

	dialCtx, dialDone := context.WithTimeout(ctx, DialTimeout)
	defer dialDone()

	client, err := link.Dial(dialCtx, opts.Endpoint)
	if err != nil {
		return protocol.Response{}, fmt.Errorf("%w: %w", ErrNoService, err)
	}

	defer client.Close()

	_, err = ask(callCtx, client, protocol.Request{ //nolint:exhaustruct // приветствию хватает роли, вида и сессии
		Op:      protocol.OpHello,
		Role:    protocol.RoleTool,
		Kind:    string(protocol.KindTool),
		Session: opts.Session,
	})
	if err != nil {
		return protocol.Response{}, err
	}

	req.Session = opts.Session

	return ask(callCtx, client, req)
}

// ask отправляет кадр и переводит отказ службы в ошибку с её кодом. Всё, что случилось после
// соединения, — молчание службы (`ErrSilent`), а не её отсутствие: соединение состоялось, значит
// слушающий есть.
func ask(ctx context.Context, client *link.Client, req protocol.Request) (protocol.Response, error) {
	resp, err := client.Call(ctx, req)
	if err != nil {
		return protocol.Response{}, fmt.Errorf("%w: %w", ErrSilent, err)
	}

	if !resp.OK {
		return resp, refusal(resp)
	}

	return resp, nil
}

// refusal собирает ошибку из отказа службы: код контракта и её текст.
func refusal(resp protocol.Response) error {
	if resp.Error == nil {
		return fmt.Errorf("%w: отказ без кода контракта", ErrRefused)
	}

	return fmt.Errorf("%w: %s: %s", ErrRefused, resp.Error.Code, resp.Error.Text)
}
