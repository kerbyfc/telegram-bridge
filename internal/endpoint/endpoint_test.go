package endpoint //nolint:testpackage // проверяются неэкспортированные построители имени канала и его прав

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPipeNameFromSettings: имя канала строится из того же ключа настроек, что путь сокета, —
// короткое имя получает начало, полное остаётся собой.
func TestPipeNameFromSettings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		configured string
		want       string
	}{
		{name: "короткое имя", configured: "dm-bridge", want: `\\.\pipe\dm-bridge`},
		{name: "полное имя", configured: `\\.\pipe\dm-bridge`, want: `\\.\pipe\dm-bridge`},
	}

	for _, one := range cases {
		got, err := pipeName(one.configured)
		if err != nil {
			t.Fatalf("%s: имя не собрано: %v", one.name, err)
		}

		if got != one.want {
			t.Fatalf("%s: имя %q, ждали %q", one.name, got, one.want)
		}
	}
}

// TestPipeNameRefusesPath: путь в ключе `socket_path` — отказ с причиной, а не канал со странным
// именем: так настройки, перенесённые с macOS без правки, называют себя сами.
func TestPipeNameRefusesPath(t *testing.T) {
	t.Parallel()

	_, err := pipeName("")
	if !errors.Is(err, ErrName) {
		t.Fatalf("пустое имя принято каналом: %v", err)
	}

	for _, configured := range []string{
		"/tmp/мост/bridge.sock",
		`C:\Users\owner\bridge.sock`,
		`~\bridge.sock`,
	} {
		_, err := pipeName(configured)
		if !errors.Is(err, ErrName) {
			t.Fatalf("%q принят именем канала: %v", configured, err)
		}

		if !strings.Contains(err.Error(), "канал носит имя") {
			t.Fatalf("%q: отказ не говорит, что именно не так: %v", configured, err)
		}
	}
}

// TestDefaultPipeNameFromStateRoot: имя по умолчанию выводится из корня состояния, поэтому у службы
// ветки со своим корнем свой канал; один и тот же корень даёт одно и то же имя.
func TestDefaultPipeNameFromStateRoot(t *testing.T) {
	t.Parallel()

	first := defaultPipeName("/состояние/мостика")
	if first != defaultPipeName("/состояние/мостика/") || first != defaultPipeName("/состояние/./мостика") {
		t.Fatal("один корень состояния дал разные имена канала")
	}

	if first == defaultPipeName("/состояние/другого-мостика") {
		t.Fatal("разные корни состояния дали одно имя канала: службы веток вытесняли бы друг друга")
	}

	if !strings.HasPrefix(first, `\\.\pipe\dm-telegram-bridge-`) {
		t.Fatalf("имя канала по умолчанию не то: %q", first)
	}
}

// TestPipeSecurityDescriptor: права канала заданы явно — запись доступа без наследования и только
// на владельца. Права платформы по умолчанию открыли бы канал администраторам машины и SYSTEM.
func TestPipeSecurityDescriptor(t *testing.T) {
	t.Parallel()

	const owner = "S-1-5-21-1004336348-1177238915-682003330-1001"

	descriptor, err := pipeSecurityDescriptor(owner)
	if err != nil {
		t.Fatalf("дескриптор не собран: %v", err)
	}

	want := "D:P(A;;GA;;;" + owner + ")"
	if descriptor != want {
		t.Fatalf("дескриптор %q, ждали %q", descriptor, want)
	}

	_, err = pipeSecurityDescriptor("")
	if !errors.Is(err, ErrName) {
		t.Fatalf("канал открыт без владельца: %v", err)
	}

	_, err = pipeSecurityDescriptor("BUILTIN\\Администраторы")
	if !errors.Is(err, ErrName) {
		t.Fatalf("владельцем принято имя учётной записи, а не SID: %v", err)
	}
}

// TestDefaultAddressOfPlatform: адрес по умолчанию — свой у площадки: сокет в корне состояния на
// macOS, имя канала на Windows.
func TestDefaultAddressOfPlatform(t *testing.T) {
	t.Parallel()

	address := Default("/состояние/мостика")

	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(address, `\\.\pipe\`) {
			t.Fatalf("на Windows адрес по умолчанию не канал: %q", address)
		}

		return
	}

	if address != filepath.Join("/состояние/мостика", "bridge.sock") {
		t.Fatalf("на этой площадке адрес по умолчанию не сокет: %q", address)
	}
}

// TestListenAndDial: слушатель и соединение по адресу площадки — один транспорт для обеих.
func TestListenAndDial(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	address := Default(shortRoot(t))

	listener, err := Listen(ctx, address)
	if err != nil {
		t.Fatalf("адрес не поднят: %v", err)
	}

	defer func() { _ = listener.Close() }()

	accepted := make(chan string, 1)

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			accepted <- "соединение не принято: " + acceptErr.Error()

			return
		}

		defer func() { _ = conn.Close() }()

		line, readErr := bufio.NewReader(conn).ReadString('\n')
		if readErr != nil {
			accepted <- "кадр не прочитан: " + readErr.Error()

			return
		}

		accepted <- line
	}()

	conn, err := Dial(ctx, address)
	if err != nil {
		t.Fatalf("соединение не открыто: %v", err)
	}

	defer func() { _ = conn.Close() }()

	_, err = conn.Write([]byte("здравствуй\n"))
	if err != nil {
		t.Fatalf("кадр не отправлен: %v", err)
	}

	select {
	case line := <-accepted:
		if line != "здравствуй\n" {
			t.Fatalf("служба прочитала %q", line)
		}
	case <-ctx.Done():
		t.Fatal("служба кадра не дождалась")
	}
}

// TestSecondListenOnSameAddressIsBusy: занятый адрес — громкий отказ, а не тихий второй процесс.
func TestSecondListenOnSameAddressIsBusy(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	address := Default(shortRoot(t))

	first, err := Listen(ctx, address)
	if err != nil {
		t.Fatalf("адрес не поднят: %v", err)
	}

	_, err = Listen(ctx, address)
	if !errors.Is(err, ErrBusy) {
		_ = first.Close()

		t.Fatalf("второй слушатель встал на занятый адрес: %v", err)
	}

	_ = first.Close()

	// Освободившийся адрес снова годен: остаток прежнего запуска не мешает.
	again, err := Listen(ctx, address)
	if err != nil {
		t.Fatalf("освободившийся адрес не поднят: %v", err)
	}

	_ = again.Close()
}

// shortRoot — каталог состояния для теста. На macOS адрес Unix-сокета короче временного каталога
// теста, поэтому каталог заводится в `/tmp`.
func shortRoot(t *testing.T) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		return t.TempDir()
	}

	//nolint:usetesting // t.TempDir даёт путь длиннее предела адреса Unix-сокета на macOS
	dir, err := os.MkdirTemp("/tmp", "dmtg")
	if err != nil {
		t.Fatalf("каталог состояния не создан: %v", err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}
