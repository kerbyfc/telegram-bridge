package cli //nolint:testpackage // сверка значков неэкспортирована: проверяется на подставе Bot API

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

const iconsSet = `{"ok":true,"result":[{"emoji":"🤖","custom_emoji_id":"aa"},{"emoji":"💻","custom_emoji_id":"bb"}]}`

// iconsStand собирает настройки с названными значками и клиент к подставе, отвечающей на всё `body`.
func iconsStand(t *testing.T, iconOn, iconOff, body string) (*botapi.Client, *config.Config) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(wrt http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(wrt, body)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{TopicIconOn: iconOn, TopicIconOff: iconOff} //nolint:exhaustruct // сверке нужны только значки

	client, err := botapi.NewClient(botapi.Options{
		Token: "4242:тест", GroupID: -1001, Proxy: "", RetryPauses: []time.Duration{time.Millisecond},
		SendGap: 0, BaseURL: srv.URL, TopicIconOn: iconOn, TopicIconOff: iconOff,
	})
	if err != nil {
		t.Fatalf("клиент не собрался: %v", err)
	}

	return client, cfg
}

// deadStand — тот же стенд без сети: подстава закрыта, и Telegram недоступен.
func deadStand(t *testing.T) (*botapi.Client, *config.Config) {
	t.Helper()

	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	cfg := &config.Config{TopicIconOn: "🦄", TopicIconOff: "💻"} //nolint:exhaustruct // сверке нужны только значки

	client, err := botapi.NewClient(botapi.Options{
		Token: "4242:тест", GroupID: -1001, Proxy: "", RetryPauses: []time.Duration{time.Millisecond},
		SendGap: 0, BaseURL: url, TopicIconOn: "🦄", TopicIconOff: "💻",
	})
	if err != nil {
		t.Fatalf("клиент не собрался: %v", err)
	}

	return client, cfg
}

// journal открывает журнал во временном каталоге.
func journal(t *testing.T) *store.Journal {
	t.Helper()

	jrnl, err := store.OpenJournal(filepath.Join(t.TempDir(), "bridge.log"), config.DefaultLogMaxBytes, config.DefaultLogKeep)
	if err != nil {
		t.Fatalf("журнал не открыт: %v", err)
	}

	t.Cleanup(func() { _ = jrnl.Close() })

	return jrnl
}

// TestIconsCheckNamesKeyAndValue: значок вне набора — отказ с именем настройки и значением, каждый
// негодный значок назван.
func TestIconsCheckNamesKeyAndValue(t *testing.T) {
	t.Parallel()

	client, cfg := iconsStand(t, "🦄", "🐙", iconsSet)

	got := iconsCheck(context.Background(), client, cfg)
	if got.State != stateFailed {
		t.Fatalf("значок вне набора не отказ: %+v", got)
	}

	for _, want := range []string{config.TopicIconOnKey, "🦄", config.TopicIconOffKey, "🐙"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("в отказе нет %q: %s", want, got.Detail)
		}
	}
}

// TestIconsCheckPasses: оба значка в наборе — проверка подтверждена.
func TestIconsCheckPasses(t *testing.T) {
	t.Parallel()

	client, cfg := iconsStand(t, "🤖", "💻", iconsSet)

	if got := iconsCheck(context.Background(), client, cfg); got.State != stateOK {
		t.Fatalf("годные значки не подтверждены: %+v", got)
	}
}

// TestIconsCheckNetworkIsNotBadValue: Telegram недоступен или отказал — значение не подтверждено, но
// и не негодно, как бы оно ни выглядело.
func TestIconsCheckNetworkIsNotBadValue(t *testing.T) {
	t.Parallel()

	client, cfg := deadStand(t)

	if got := iconsCheck(context.Background(), client, cfg); got.State != stateUnknown {
		t.Fatalf("недоступность Telegram названа не «не подтверждено»: %+v", got)
	}

	refused, cfg := iconsStand(t, "🦄", "💻", `{"ok":false,"error_code":401,"description":"Unauthorized"}`)

	if got := iconsCheck(context.Background(), refused, cfg); got.State != stateUnknown {
		t.Fatalf("отказ платформы назван негодным значком: %+v", got)
	}
}

// TestIconsAtStart: значок вне набора не даёт службе подняться тем же кодом, что негодные настройки;
// недоступность Telegram поднимается и оставляет строку журнала.
func TestIconsAtStart(t *testing.T) {
	t.Parallel()

	client, cfg := iconsStand(t, "🦄", "💻", iconsSet)

	var exit exitError

	err := checkIconsAtStart(context.Background(), client, cfg, journal(t))
	if !errors.As(err, &exit) || exit.code != ExitUsage || !strings.Contains(exit.msg, config.TopicIconOnKey) {
		t.Fatalf("служба поднимается со значком вне набора: %v", err)
	}

	client, cfg = deadStand(t)
	jrnl := journal(t)

	err = checkIconsAtStart(context.Background(), client, cfg, jrnl)
	if err != nil {
		t.Fatalf("недоступность Telegram не подняла службу: %v", err)
	}

	entries, err := store.TailJournal(jrnl.Path(), 1)
	if err != nil || len(entries) != 1 || entries[0].Event != "topic-icons-unverified" {
		t.Fatalf("журнал: %+v, %v", entries, err)
	}
}
