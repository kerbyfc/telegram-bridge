//go:build windows

package resolve_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/kerbyfc/telegram-bridge/internal/resolve"
	"github.com/kerbyfc/telegram-bridge/internal/resolve/resolvetest"
)

// TestRunСрокУбиваетПотомков: по сроку объект задания убивает не только команду, но и её
// потомка, державшего поток вывода, — висящих процессов разворот не оставляет.
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

	proc, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Процесса уже нет вовсе: открыть нечего.
		return
	}
	defer windows.CloseHandle(proc) //nolint:errcheck // закрытие дескриптора исхода не меняет

	event, err := windows.WaitForSingleObject(proc, 2000)
	if err != nil || event != windows.WAIT_OBJECT_0 {
		t.Fatalf("потомок %d жив после срока: %d, %v", pid, event, err)
	}
}
