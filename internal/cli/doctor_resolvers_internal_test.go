package cli //nolint:testpackage // проверка неэкспортирована

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/config"
)

// TestResolversCheck: программа записи разворота находится — проверка пройдена; не находится —
// отказ с именем программы.
func TestResolversCheck(t *testing.T) {
	t.Parallel()

	resolver := func(program string) config.LinkResolver {
		res, err := config.NewLinkResolver(`.*`, `.*`, []string{program, "{link}"}, time.Second)
		if err != nil {
			t.Fatalf("запись не собрана: %v", err)
		}

		return res
	}

	cfg := &config.Config{LinkResolvers: []config.LinkResolver{resolver(os.Args[0])}} //nolint:exhaustruct // проверке нужны одни записи

	if got := resolversCheck(cfg); got.State != stateOK {
		t.Fatalf("найденная программа названа ненайденной: %+v", got)
	}

	cfg.LinkResolvers = append(cfg.LinkResolvers, resolver("no-such-program-for-resolve"))

	got := resolversCheck(cfg)
	if got.State != stateFailed || !strings.Contains(got.Detail, "no-such-program-for-resolve") {
		t.Fatalf("ненайденная программа не названа: %+v", got)
	}
}
