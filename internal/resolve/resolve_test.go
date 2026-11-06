package resolve_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/resolve"
	"github.com/kerbyfc/telegram-bridge/internal/resolve/resolvetest"
)

func TestMain(m *testing.M) {
	resolvetest.Serve()
	os.Exit(m.Run())
}

// resolver собирает запись разворота или валит тест.
func resolver(t *testing.T, where, link string, command []string, timeout time.Duration) config.LinkResolver {
	t.Helper()

	res, err := config.NewLinkResolver(where, link, command, timeout)
	if err != nil {
		t.Fatalf("запись не собрана: %v", err)
	}

	return res
}

// links отдаёт ссылки найденного по порядку.
func links(hits []resolve.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.Link)
	}

	return out
}

// TestFindВГруппеМеста: есть группа у места — ссылка ищется только в ней; ссылка той же формы вне
// места не берётся.
func TestFindВГруппеМеста(t *testing.T) {
	t.Parallel()

	res := resolver(t, `(?m)^needs? input:(.*)$`, `x://[^\s)]+`, resolvetest.Command(resolvetest.ModePrint, "{link}"), time.Second)
	text := "смотри x://вне\nneeds input: [а](x://один), [б](x://два)\nи x://после"

	got := links(resolve.Find(text, []config.LinkResolver{res}))
	if strings.Join(got, ",") != "x://один,x://два" {
		t.Fatalf("ссылки места не те: %v", got)
	}
}

// TestFindБезГруппы: без группы ссылка ищется во всём совпадении места, с его рамкой.
func TestFindБезГруппы(t *testing.T) {
	t.Parallel()

	res := resolver(t, `<ask>x://[^<]*</ask>`, `x://[^<]+`, resolvetest.Command(resolvetest.ModePrint, "{link}"), time.Second)

	got := links(resolve.Find("до <ask>x://раз</ask> и x://мимо", []config.LinkResolver{res}))
	if strings.Join(got, ",") != "x://раз" {
		t.Fatalf("ссылки не те: %v", got)
	}
}

// TestFindПорядокТекста: ссылки нескольких записей идут по порядку текста, а не записей.
func TestFindПорядокТекста(t *testing.T) {
	t.Parallel()

	cmd := resolvetest.Command(resolvetest.ModePrint, "{link}")
	first := resolver(t, `\[\[(.+?)\]\]`, `.+`, cmd, time.Second)
	second := resolver(t, `(?s)<ask>(.*?)</ask>`, `\S+`, cmd, time.Second)

	got := links(resolve.Find("<ask>a b</ask> [[c]] <ask>d</ask>", []config.LinkResolver{first, second}))
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Fatalf("порядок не тот: %v", got)
	}
}

// TestFindБезЗаписей: записей нет — ссылок нет.
func TestFindБезЗаписей(t *testing.T) {
	t.Parallel()

	if hits := resolve.Find("[[a]]", nil); len(hits) != 0 {
		t.Fatalf("без записей нашлось: %v", hits)
	}
}

// TestRunВывод: вывод команды приходит с переводами строк `\n`, без хвостового перевода, ссылка —
// одним аргументом, даже с пробелами и знаками оболочки.
func TestRunВывод(t *testing.T) {
	t.Parallel()

	res := resolver(t, `.*`, `.*`, resolvetest.Command(resolvetest.ModePrint, "{link}"), 10*time.Second)

	out, err := resolve.Run(context.Background(), res, "a b; $(c) | d")
	if err != nil {
		t.Fatalf("разворот отказал: %v", err)
	}

	if out != "развёрнуто a b; $(c) | d\nвторая строка" {
		t.Fatalf("вывод не тот: %q", out)
	}
}

// TestRunОтказы: каждый негодный исход — отказ с причиной.
func TestRunОтказы(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		mode   string
		reason string
	}{
		{"код выхода", resolvetest.ModeFail, "код выхода 3: нет такой заметки"},
		{"пустой вывод", resolvetest.ModeEmpty, "команда ничего не вывела"},
		{"не UTF-8", resolvetest.ModeBadUTF8, "вывод не в UTF-8"},
	}

	for _, one := range cases {
		t.Run(one.name, func(t *testing.T) {
			t.Parallel()

			res := resolver(t, `.*`, `.*`, resolvetest.Command(one.mode, "{link}"), 10*time.Second)

			_, err := resolve.Run(context.Background(), res, "x")

			var failure *resolve.Failure
			if !errors.As(err, &failure) || failure.Reason != one.reason {
				t.Fatalf("отказ не тот: %v", err)
			}
		})
	}
}

// TestRunНетПрограммы: программы нет — отказ запуска, а не паника.
func TestRunНетПрограммы(t *testing.T) {
	t.Parallel()

	res := resolver(t, `.*`, `.*`, []string{"no-such-program-for-resolve", "{link}"}, time.Second)

	_, err := resolve.Run(context.Background(), res, "x")
	if err == nil || !strings.HasPrefix(err.Error(), "команда не запущена") {
		t.Fatalf("отказ не тот: %v", err)
	}
}

// TestRunСрок: команда, не уложившаяся в срок, убита, и причина называет срок.
func TestRunСрок(t *testing.T) {
	t.Parallel()

	res := resolver(t, `.*`, `.*`, resolvetest.Command(resolvetest.ModeSleep, "{link}"), 200*time.Millisecond)
	started := time.Now()

	_, err := resolve.Run(context.Background(), res, "x")
	if err == nil || err.Error() != "срок 200ms вышел" {
		t.Fatalf("отказ не тот: %v", err)
	}

	if spent := time.Since(started); spent > 5*time.Second {
		t.Fatalf("срок не удержан: %s", spent)
	}
}
