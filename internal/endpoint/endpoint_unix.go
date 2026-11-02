//go:build !windows

package endpoint

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

const (
	// socketPerm — права сокета: единственная настоящая граница доступа здесь пользователь системы.
	socketPerm os.FileMode = 0o600
	// dirPerm — права каталога сокета: в него не заходит никто, кроме владельца.
	dirPerm os.FileMode = 0o700
	// frontSocket — имя сокета местного входа внутри своего каталога. Каталог заводится на один
	// запуск подкоманды, поэтому имя одно и то же: два мода одной сессии не делят ни каталог, ни
	// сокет.
	frontSocket = "l.sock"
)

// Default — адрес границы по умолчанию: сокет в корне состояния.
func Default(stateRoot string) string {
	return filepath.Join(stateRoot, "bridge.sock")
}

// Word — как называется адрес этой площадки в сообщениях владельцу.
func Word() string {
	return "сокет"
}

// Listen поднимает сокет службы: каталог 0700, сокет 0600. Оставшийся от прежнего запуска сокет
// без живого слушателя убирается, занятый — громкий отказ, а не тихий второй процесс.
func Listen(ctx context.Context, address string) (net.Listener, error) {
	err := os.MkdirAll(filepath.Dir(address), dirPerm)
	if err != nil {
		return nil, fmt.Errorf("каталог сокета не создан: %w", err)
	}

	err = clearStale(ctx, address)
	if err != nil {
		return nil, err
	}

	cfg := net.ListenConfig{} //nolint:exhaustruct // слушателю сокета хватает значений по умолчанию

	listener, err := cfg.Listen(ctx, "unix", address)
	if err != nil {
		return nil, fmt.Errorf("сокет не поднят: %w", err)
	}

	err = os.Chmod(address, socketPerm)
	if err != nil {
		_ = listener.Close()

		return nil, fmt.Errorf("права сокета не выставлены: %w", err)
	}

	return listener, nil
}

// Dial открывает соединение с сокетом службы.
func Dial(ctx context.Context, address string) (net.Conn, error) {
	dialer := net.Dialer{} //nolint:exhaustruct // соединению хватает значений по умолчанию

	conn, err := dialer.DialContext(ctx, "unix", address)
	if err != nil {
		return nil, fmt.Errorf("%s %s не отвечает: %w", Word(), address, err)
	}

	return conn, nil
}

// Front поднимает местный вход подкоманды `link`: свой каталог на один запуск и сокет в нём.
// Каталог заводится заново каждым запуском, поэтому занятого сокета и остатков прежнего тут не
// бывает. Вторая половина ответа — уборка за собой.
func Front(ctx context.Context) (net.Listener, func(), error) {
	dir, err := os.MkdirTemp("", "dmtg-")
	if err != nil {
		return nil, nil, fmt.Errorf("каталог местного входа не создан: %w", err)
	}

	cleanup := func() { _ = os.RemoveAll(dir) }

	err = os.Chmod(dir, dirPerm)
	if err != nil {
		cleanup()

		return nil, nil, fmt.Errorf("права каталога местного входа не выставлены: %w", err)
	}

	cfg := net.ListenConfig{} //nolint:exhaustruct // слушателю местного входа хватает значений по умолчанию

	listener, err := cfg.Listen(ctx, "unix", filepath.Join(dir, frontSocket))
	if err != nil {
		cleanup()

		return nil, nil, fmt.Errorf("местный вход не поднят: %w", err)
	}

	return listener, cleanup, nil
}

// clearStale убирает сокет, оставшийся от прежнего запуска: живой слушатель отвечает на
// соединение, мёртвый — нет.
func clearStale(ctx context.Context, address string) error {
	_, err := os.Stat(address)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	dialer := net.Dialer{} //nolint:exhaustruct // пробе соединения хватает значений по умолчанию

	probe, err := dialer.DialContext(ctx, "unix", address)
	if err == nil {
		_ = probe.Close()

		return fmt.Errorf("%w: сокет %s", ErrBusy, address)
	}

	err = os.Remove(address)
	if err != nil {
		return fmt.Errorf("прежний сокет не убран: %w", err)
	}

	return nil
}
