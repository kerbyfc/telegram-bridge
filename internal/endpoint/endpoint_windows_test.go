//go:build windows

package endpoint_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/endpoint"
)

// TestCurrentOwnerIsSID: права канала строятся по SID владельца процесса, а не по имени учётной
// записи: имя локализовано, SID — адрес.
func TestCurrentOwnerIsSID(t *testing.T) {
	t.Parallel()

	owner, err := endpoint.CurrentOwner()
	if err != nil {
		t.Fatalf("владелец процесса не прочитан: %v", err)
	}

	if !strings.HasPrefix(owner, "S-1-") {
		t.Fatalf("владелец процесса назван не SID: %q", owner)
	}
}

// TestSecondListenOnPipeIsBusy: занятое имя канала называется занятым адресом, а не отказом создания.
// Занятость приходит от ядра: go-winio создаёт первый дескриптор с `FILE_CREATE`, и на живое имя
// приходит `STATUS_OBJECT_NAME_COLLISION` → `ERROR_ALREADY_EXISTS`.
func TestSecondListenOnPipeIsBusy(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	address := endpoint.Default(t.TempDir())

	first, err := endpoint.Listen(ctx, address)
	if err != nil {
		t.Fatalf("канал не поднят: %v", err)
	}

	defer func() { _ = first.Close() }()

	_, err = endpoint.Listen(ctx, address)
	if !errors.Is(err, endpoint.ErrBusy) {
		t.Fatalf("второй слушатель встал на занятый канал: %v", err)
	}
}

// TestFrontNamesDiffer: у каждого запуска подкоманды свой местный вход — каналы живут в общем
// пространстве имён машины.
func TestFrontNamesDiffer(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	first, err := endpoint.Front(ctx)
	if err != nil {
		t.Fatalf("первый местный вход не поднят: %v", err)
	}

	defer first.Close()

	second, err := endpoint.Front(ctx)
	if err != nil {
		t.Fatalf("второй местный вход не поднят: %v", err)
	}

	defer second.Close()

	if first.Local.Addr().String() == second.Local.Addr().String() {
		t.Fatalf("два местных входа получили один адрес: %q", first.Local.Addr().String())
	}

	if first.Loopback.Addr().String() == second.Loopback.Addr().String() {
		t.Fatalf("два входа на loopback получили один адрес: %q", first.Loopback.Addr().String())
	}
}

// TestFrontHasLoopback: на Windows у подкоманды два входа — канал и HTTP-вход на `127.0.0.1` со
// своим портом: хозяин мода Claude Code в канал не ходит.
func TestFrontHasLoopback(t *testing.T) {
	t.Parallel()

	fronts, err := endpoint.Front(t.Context())
	if err != nil {
		t.Fatalf("местные входы не подняты: %v", err)
	}

	defer fronts.Close()

	if !strings.HasPrefix(fronts.Local.Addr().String(), `\\.\pipe\dm-telegram-link-`) {
		t.Fatalf("вход площадки не канал: %q", fronts.Local.Addr().String())
	}

	if fronts.Loopback == nil {
		t.Fatal("входа на loopback нет")
	}

	address := fronts.Loopback.Addr().String()
	if !strings.HasPrefix(address, "127.0.0.1:") || strings.HasSuffix(address, ":0") {
		t.Fatalf("вход на loopback не на своём порту 127.0.0.1: %q", address)
	}
}
