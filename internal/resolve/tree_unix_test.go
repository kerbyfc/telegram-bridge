//go:build !windows

package resolve_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/resolve"
	"github.com/kerbyfc/telegram-bridge/internal/resolve/resolvetest"
)

// TestRunСрокУбиваетПотомков: по сроку умирает не только команда, но и её потомок, державший
// поток вывода, — висящих процессов разворот не оставляет.
func TestRunСрокУбиваетПотомков(t *testing.T) {
	t.Parallel()

	pidFile := filepath.Join(t.TempDir(), "pid")
	res := resolver(t, `.*`, `.*`, resolvetest.Command(resolvetest.ModeSpawn, pidFile, "{link}"), 500*time.Millisecond)
	started := time.Now()

	_, err := resolve.Run(context.Background(), res, "x")
	if err == nil {
		t.Fatal("спящая команда не отказала")
	}

	if spent := time.Since(started); spent > 3*time.Second {
		t.Fatalf("потомок держал разворот: %s", spent)
	}

	body, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("pid потомка не записан: %v", err)
	}

	pid, err := strconv.Atoi(string(body))
	if err != nil {
		t.Fatalf("pid потомка не разобран: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("потомок %d жив после срока", pid)
		}

		time.Sleep(20 * time.Millisecond)
	}
}
