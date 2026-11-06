package cli //nolint:testpackage // проверка неэкспортирована

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestResolversCheck: пути программ называет служба; службы нет — «не подтверждено», а не
// проверка по PATH терминала; записи службы разошлись с файлом — тоже «не подтверждено».
func TestResolversCheck(t *testing.T) {
	t.Parallel()

	res, err := config.NewLinkResolver(`.*`, `.*`, []string{"tracker", "{link}"}, time.Second)
	if err != nil {
		t.Fatalf("запись не собрана: %v", err)
	}

	cfg := &config.Config{LinkResolvers: []config.LinkResolver{res}} //nolint:exhaustruct // проверке нужны одни записи

	if got := resolversCheck(cfg, nil); got.State != stateUnknown || !strings.Contains(got.Detail, "службы нет") {
		t.Fatalf("без службы проверка не та: %+v", got)
	}

	status := &protocol.ServiceStatus{ //nolint:exhaustruct // проверке нужны одни записи
		Resolvers: []protocol.ResolverStatus{{Program: "tracker", Path: "/opt/bin/tracker"}},
	}

	got := resolversCheck(cfg, status)
	if got.State != stateOK || !strings.Contains(got.Detail, "tracker → /opt/bin/tracker") {
		t.Fatalf("путь службы не назван: %+v", got)
	}

	status.Resolvers[0].Program = os.Args[0]

	if got = resolversCheck(cfg, status); got.State != stateUnknown || !strings.Contains(got.Detail, "перезапусти") {
		t.Fatalf("расхождение со службой не названо: %+v", got)
	}
}
