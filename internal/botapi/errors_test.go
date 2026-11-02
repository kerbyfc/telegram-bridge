package botapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// floodBody — отказ флуд-лимита со сроком, который называет сам Telegram.
const floodBody = `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1",` +
	`"parameters":{"retry_after":1}}`

// TestFloodWaitsNamedTimeAndRetriesOnce: флуд-лимит — не пропавшая тема и не сеть, а пауза: клиент
// ждёт названный Telegram срок и повторяет вызов один раз.
func TestFloodWaitsNamedTimeAndRetriesOnce(t *testing.T) {
	t.Parallel()

	harness := newRig(t, func(num int, _ string) reply {
		if num == 1 {
			return answered(http.StatusTooManyRequests, floodBody)
		}

		return answered(http.StatusOK, okMessage)
	})

	started := time.Now()

	id, err := harness.client(t).SendMessage(context.Background(), 42, "слово")
	if err != nil {
		t.Fatalf("повтор по сроку не прошёл: %v", err)
	}

	spent := time.Since(started)

	if id != 11 {
		t.Fatalf("id сообщения %d, а подстава назвала 11", id)
	}

	if spent < time.Second {
		t.Fatalf("срок повтора не выждан: ушло %v", spent)
	}

	if len(harness.recorded()) != 2 {
		t.Fatalf("вызовов %d, а ждали два", len(harness.recorded()))
	}
}

// TestFloodRetriedOnlyOnce: второй флуд-лимит подряд повтором не переживается — ждать по кругу
// значит держать ход службы, — и отказ доходит до звонящего своим кодом со своим сроком.
func TestFloodRetriedOnlyOnce(t *testing.T) {
	t.Parallel()

	harness := newRig(t, func(_ int, _ string) reply {
		return answered(http.StatusTooManyRequests, floodBody)
	})

	_, err := harness.client(t).SendMessage(context.Background(), 42, "слово")
	if err == nil {
		t.Fatal("флуд-лимит остался без отказа")
	}

	if botapi.Code(err) != protocol.CodeRateLimited {
		t.Fatalf("код отказа %s, а ждали rate-limited: %v", botapi.Code(err), err)
	}

	if botapi.RetryAfter(err) != time.Second {
		t.Fatalf("срок повтора в отказе: %v", botapi.RetryAfter(err))
	}

	if len(harness.recorded()) != 2 {
		t.Fatalf("вызовов %d, а повтор флуд-лимита один", len(harness.recorded()))
	}
}

// TestNetworkRetriedByPausesThenNamed: сетевой отказ переживается паузами повтора по порядку, а
// после всех повторов называется своим кодом.
func TestNetworkRetriedByPausesThenNamed(t *testing.T) {
	t.Parallel()

	harness := newRig(t, func(_ int, _ string) reply {
		return broken()
	})

	_, err := harness.client(t).SendMessage(context.Background(), 42, "слово")
	if err == nil {
		t.Fatal("оборванное соединение осталось без отказа")
	}

	if botapi.Code(err) != protocol.CodeNetwork {
		t.Fatalf("код отказа %s, а ждали network: %v", botapi.Code(err), err)
	}

	// Две паузы повтора у клиента подставы — значит три попытки всего.
	if len(harness.recorded()) != 3 {
		t.Fatalf("попыток %d, а паузы повтора две", len(harness.recorded()))
	}
}

// TestNetworkRefusalHidesToken: строка сетевого отказа уходит в журнал службы и в кадр отказа
// звонящему, а `*url.Error` от `http.Client.Do` несёт адрес вызова с токеном бота в пути. В строке
// отказа остаются метод и причина, токена нет.
func TestNetworkRefusalHidesToken(t *testing.T) {
	t.Parallel()

	harness := newRig(t, func(_ int, _ string) reply {
		return broken()
	})

	_, err := harness.client(t).SendMessage(context.Background(), 42, "слово")
	if err == nil {
		t.Fatal("оборванное соединение осталось без отказа")
	}

	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("токен бота в строке отказа: %v", err)
	}

	if !strings.Contains(err.Error(), "/bot…/sendMessage") {
		t.Fatalf("строка отказа не называет метод: %v", err)
	}
}

// TestNoTopicNotRetried: пропавшая тема узнаётся только по описанию отказа, повтором не
// переживается — её чинит заведением темы заново ядро.
func TestNoTopicNotRetried(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(
		`{"ok":false,"error_code":400,"description":"Bad Request: message thread not found"}`,
	))

	_, err := harness.client(t).SendMessage(context.Background(), 42, "слово")
	if !botapi.IsNoTopic(err) {
		t.Fatalf("удалённая тема не узнана: %v", err)
	}

	if len(harness.recorded()) != 1 {
		t.Fatalf("вызовов %d, а повторять пропавшую тему незачем", len(harness.recorded()))
	}
}

// TestRefusalNamesPlatformCause: обычный отказ платформы не повторяется впустую и доносит до
// звонящего описание Telegram, а не «сеть не отвечает».
func TestRefusalNamesPlatformCause(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(
		`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`,
	))

	_, err := harness.client(t).SendMessage(context.Background(), 42, "слово")
	if botapi.Code(err) != protocol.CodeTelegramRefused {
		t.Fatalf("код отказа %s, а ждали telegram-refused: %v", botapi.Code(err), err)
	}

	if !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("отказ не донёс причину платформы: %v", err)
	}

	if len(harness.recorded()) != 1 {
		t.Fatalf("вызовов %d, а отказ платформы впустую не повторяется", len(harness.recorded()))
	}
}

// TestUnparsedAnswerNamedRefusal: ответ не кадром Bot API (перехватчик, шлюз) называется отказом
// с началом ответа в тексте — повторять его незачем, а причина видна в журнале.
func TestUnparsedAnswerNamedRefusal(t *testing.T) {
	t.Parallel()

	harness := newRig(t, func(_ int, _ string) reply {
		return answered(http.StatusBadGateway, "<html>proxy</html>")
	})

	_, err := harness.client(t).SendMessage(context.Background(), 42, "слово")
	if botapi.Code(err) != protocol.CodeTelegramRefused {
		t.Fatalf("код отказа %s, а ждали telegram-refused: %v", botapi.Code(err), err)
	}

	if !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("отказ не донёс начало ответа: %v", err)
	}
}

// TestStopCarriesContractCode: останов службы на паузе повтора доходит отказом с кодом
// контракта — отказ без кода ядро считает нарушенным инвариантом и падает на нём.
func TestStopCarriesContractCode(t *testing.T) {
	t.Parallel()

	harness := newRig(t, func(_ int, _ string) reply {
		return answered(http.StatusTooManyRequests, floodBody)
	})

	ctx, stop := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		stop()
	}()

	_, err := harness.client(t).SendMessage(ctx, 42, "слово")
	if err == nil {
		t.Fatal("прерванный вызов остался без отказа")
	}

	if botapi.Code(err) != protocol.CodeNetwork || !strings.Contains(err.Error(), "прерван") {
		t.Fatalf("отказ прерванного вызова: %v", err)
	}

	// Причина доходит сквозь код контракта: на этой проверке стоит ветка поллера, которая на
	// штатном останове выходит молча, вместо записи «сеть не отвечает» в журнал.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("останов не узнаётся по причине: %v", err)
	}
}

// TestOversizeAnswerNamedNotTruncated: ответ больше предела чтения называется отказом, а не
// режется молча. Молча усечённый кадр не разобрался бы, смещение опроса не двинулось бы, и
// служба просила бы ту же непролезающую пачку вечно — канал встал бы навсегда.
func TestOversizeAnswerNamedNotTruncated(t *testing.T) {
	t.Parallel()

	harness := newRig(t, func(_ int, _ string) reply {
		return answered(http.StatusOK, strings.Repeat("x", botapi.MaxBodySize+1))
	})

	_, err := harness.client(t).SendMessage(context.Background(), 42, "слово")
	if err == nil {
		t.Fatal("ответ больше предела чтения прошёл молча")
	}

	if !strings.Contains(err.Error(), "больше предела чтения") {
		t.Fatalf("отказ не называет причину: %v", err)
	}
}
