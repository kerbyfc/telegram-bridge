package core_test

import (
	"context"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestServiceStatusNamesLivenessAndSessions: самопроверка спрашивает саму службу — с какого
// времени она идёт и что знает о сессиях. Живость сессии держит открытое соединение, а не номер
// процесса, и снаружи её видно только отсюда.
func TestServiceStatusNamesLivenessAndSessions(t *testing.T) {
	t.Parallel()

	stand := newRig(t)
	conn, _ := stand.mod(modeSession, topicAlpha)

	resp := stand.do(stand.tool(""), stand.req(protocol.OpServiceStatus))
	if !resp.OK || resp.Service == nil {
		t.Fatalf("самопроверка не получила состояния службы: %+v", resp)
	}

	if resp.Service.Version != protocol.Version || resp.Service.StartedAt == "" {
		t.Fatalf("служба не назвала себя: %+v", resp.Service)
	}

	if len(resp.Service.Sessions) != 1 {
		t.Fatalf("сессий в ответе %d, а служба знает одну: %+v",
			len(resp.Service.Sessions), resp.Service.Sessions)
	}

	named := resp.Service.Sessions[0]
	want := protocol.SessionStatus{
		ID: modeSession, Client: "claude-code", Mode: protocol.ModeOn, Life: protocol.LifeAlive,
		TopicID: named.TopicID, TopicName: topicAlpha, Inbox: 0,
	}

	if named != want {
		t.Fatalf("сессия названа неверно: %+v, а ждали %+v", named, want)
	}

	// Соединение закрылось — сессия перестала быть живой, и самопроверка это видит: отсрочка
	// возврата её ещё не похоронила, но живой она уже не числится.
	stand.closeConn(conn)

	resp = stand.do(stand.tool(""), stand.req(protocol.OpServiceStatus))
	if resp.Service.Sessions[0].Life == protocol.LifeAlive {
		t.Fatalf("оборвавшаяся сессия числится живой: %+v", resp.Service.Sessions[0])
	}
}

// TestServiceStatusTellsPollOutcome: исход последнего опроса — то же, что живая проверка токена,
// группы и канала к Telegram: самопроверка берёт его отсюда и второго вызова платформы не делает.
func TestServiceStatusTellsPollOutcome(t *testing.T) {
	t.Parallel()

	stand := newRig(t)

	resp := stand.do(stand.tool(""), stand.req(protocol.OpServiceStatus))
	if resp.Service.Poll.At != "" {
		t.Fatalf("опроса ещё не было, а служба назвала его исход: %+v", resp.Service.Poll)
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	go stand.core.Poll(ctx)

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		resp = stand.do(stand.tool(""), stand.req(protocol.OpServiceStatus))
		if resp.Service.Poll.At != "" {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	stop()

	if !resp.Service.Poll.OK || resp.Service.Poll.At == "" {
		t.Fatalf("удачный опрос не отмечен: %+v", resp.Service.Poll)
	}
}
