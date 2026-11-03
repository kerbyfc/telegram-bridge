package botapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
)

// Значения подставы: токен настоящей формы — по его началу клиент узнаёт сам себя, — id группы и
// id владельца.
const (
	testToken = "4242:AAHtest-token"
	selfID    = int64(4242)
	groupID   = int64(-1001234567890)
	ownerID   = int64(777)

	testIconOn  = "🤖"
	testIconOff = "💻"
)

// Ответы Bot API, которые подстава отдаёт чаще прочих.
const (
	okMessage  = `{"ok":true,"result":{"message_id":11}}`
	okTrue     = `{"ok":true,"result":true}`
	okTopic    = `{"ok":true,"result":{"message_thread_id":501}}`
	okStickers = `{"ok":true,"result":[{"emoji":"📈","custom_emoji_id":"aa"},` +
		`{"emoji":"🤖","custom_emoji_id":"bb"},{"emoji":"💻","custom_emoji_id":"cc"}]}`
)

// reply — ответ подставы на один вызов: кадр со своим статусом или обрыв соединения вместо
// ответа — так выглядит сетевой отказ.
type reply struct {
	status int
	body   string
	broken bool
}

// call — записанный вызов клиента: имя метода Bot API и разобранный кадр.
type call struct {
	method string
	body   map[string]any
}

// rig — подстава Bot API: записывает вызовы и отвечает тем, что назначил тест.
type rig struct {
	mu     sync.Mutex
	calls  []call
	answer func(num int, method string) reply
	srv    *httptest.Server
}

// handle записывает вызов и отдаёт назначенный тестом ответ.
func (r *rig) handle(wrt http.ResponseWriter, req *http.Request) {
	body := map[string]any{}
	_ = json.NewDecoder(req.Body).Decode(&body)

	r.mu.Lock()
	r.calls = append(r.calls, call{method: path.Base(req.URL.Path), body: body})
	num := len(r.calls)
	r.mu.Unlock()

	answer := r.answer(num, path.Base(req.URL.Path))
	if answer.broken {
		r.breakConn(wrt)

		return
	}

	wrt.WriteHeader(answer.status)
	_, _ = io.WriteString(wrt, answer.body)
}

// breakConn рвёт соединение вместо ответа: так клиент видит сетевой отказ.
func (r *rig) breakConn(wrt http.ResponseWriter) {
	hijacker, ok := wrt.(http.Hijacker)
	if !ok {
		return
	}

	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}

	_ = conn.Close()
}

// recorded отдаёт записанные вызовы.
func (r *rig) recorded() []call {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]call(nil), r.calls...)
}

// client собирает клиент к подставе: без паузы между куском и куском и с быстрыми паузами
// повтора, чтобы тест не ждал настоящих сроков.
func (r *rig) client(t *testing.T) *botapi.Client {
	t.Helper()

	return r.clientWith(t, 0)
}

// clientWith собирает клиент к подставе с названной паузой между куском и куском.
func (r *rig) clientWith(t *testing.T, gap time.Duration) *botapi.Client {
	t.Helper()

	client, err := botapi.NewClient(botapi.Options{
		Token:        testToken,
		GroupID:      groupID,
		Proxy:        "",
		RetryPauses:  []time.Duration{time.Millisecond, time.Millisecond},
		SendGap:      gap,
		BaseURL:      r.srv.URL,
		TopicIconOn:  testIconOn,
		TopicIconOff: testIconOff,
	})
	if err != nil {
		t.Fatalf("клиент не собрался: %v", err)
	}

	return client
}

// newRig поднимает подставу Bot API.
func newRig(t *testing.T, answer func(num int, method string) reply) *rig {
	t.Helper()

	harness := &rig{mu: sync.Mutex{}, calls: nil, answer: answer, srv: nil}
	harness.srv = httptest.NewServer(http.HandlerFunc(harness.handle))
	t.Cleanup(harness.srv.Close)

	return harness
}

// answered — ответ подставы кадром со своим статусом.
func answered(status int, body string) reply {
	return reply{status: status, body: body, broken: false}
}

// broken — обрыв соединения вместо ответа.
func broken() reply {
	return reply{status: 0, body: "", broken: true}
}

// always отвечает одним и тем же на любой вызов.
func always(body string) func(int, string) reply {
	return func(_ int, _ string) reply {
		return answered(http.StatusOK, body)
	}
}

// byMethod отвечает по имени метода; метода нет в списке — подстава отвечает отказом, и тест
// падает на нём.
func byMethod(bodies map[string]string) func(int, string) reply {
	return func(_ int, method string) reply {
		body, ok := bodies[method]
		if !ok {
			return answered(http.StatusBadRequest,
				`{"ok":false,"error_code":400,"description":"метод подставе не назначен"}`)
		}

		return answered(http.StatusOK, body)
	}
}

// field достаёт поле кадра вызова.
func field(t *testing.T, body map[string]any, name string) any {
	t.Helper()

	value, ok := body[name]
	if !ok {
		t.Fatalf("в кадре нет поля %s: %v", name, body)
	}

	return value
}

// text достаёт строковое поле кадра вызова.
func text(t *testing.T, body map[string]any, name string) string {
	t.Helper()

	value, ok := field(t, body, name).(string)
	if !ok {
		t.Fatalf("поле %s не строка: %v", name, body[name])
	}

	return value
}

// number достаёт числовое поле кадра вызова.
func number(t *testing.T, body map[string]any, name string) float64 {
	t.Helper()

	value, ok := field(t, body, name).(float64)
	if !ok {
		t.Fatalf("поле %s не число: %v", name, body[name])
	}

	return value
}
