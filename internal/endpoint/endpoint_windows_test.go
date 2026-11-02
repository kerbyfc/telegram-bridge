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

	first, firstCleanup, err := endpoint.Front(ctx)
	if err != nil {
		t.Fatalf("первый местный вход не поднят: %v", err)
	}

	defer func() { _ = first.Close(); firstCleanup() }()

	second, secondCleanup, err := endpoint.Front(ctx)
	if err != nil {
		t.Fatalf("второй местный вход не поднят: %v", err)
	}

	defer func() { _ = second.Close(); secondCleanup() }()

	if first.Addr().String() == second.Addr().String() {
		t.Fatalf("два местных входа получили один адрес: %q", first.Addr().String())
	}
}
