//go:build windows

package endpoint

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// frontPrefix — начало имени местного входа подкоманды `link`. Имя своё у каждого запуска: каналы
// живут в общем пространстве имён машины, и два мода одной сессии не должны делить один вход.
const frontPrefix = "dm-telegram-link-"

// Default — адрес границы по умолчанию: имя канала, выведенное из корня состояния.
func Default(stateRoot string) string {
	return defaultPipeName(stateRoot)
}

// Word — как называется адрес этой площадки в сообщениях владельцу.
func Word() string {
	return "канал"
}

// Listen поднимает именованный канал службы. Права канала задаются здесь явно: дескриптор
// безопасности в SDDL разрешает полный доступ только владельцу и снимает наследование профиля.
// Права по умолчанию платформы канал открыли бы администраторам машины и SYSTEM, а граница здесь
// — пользователь системы.
func Listen(ctx context.Context, address string) (net.Listener, error) {
	err := ctx.Err()
	if err != nil {
		return nil, fmt.Errorf("канал не поднят: %w", err)
	}

	name, err := pipeName(address)
	if err != nil {
		return nil, err
	}

	owner, err := CurrentOwner()
	if err != nil {
		return nil, err
	}

	descriptor, err := pipeSecurityDescriptor(owner)
	if err != nil {
		return nil, err
	}

	listener, err := winio.ListenPipe(name, &winio.PipeConfig{
		SecurityDescriptor: descriptor,
		MessageMode:        false,
		InputBufferSize:    0,
		OutputBufferSize:   0,
	})
	if err != nil {
		return nil, listenFailure(name, err)
	}

	return listener, nil
}

// Dial открывает соединение с каналом службы.
func Dial(ctx context.Context, address string) (net.Conn, error) {
	name, err := pipeName(address)
	if err != nil {
		return nil, err
	}

	conn, err := winio.DialPipeContext(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("%s %s не отвечает: %w", Word(), name, err)
	}

	return conn, nil
}

// Front поднимает местный вход подкоманды `link`: канал со своим именем на один запуск. Канал
// живёт, пока живёт слушатель, поэтому убирать за собой нечего.
func Front(ctx context.Context) (net.Listener, func(), error) {
	token, err := randomToken()
	if err != nil {
		return nil, nil, err
	}

	listener, err := Listen(ctx, frontPrefix+token)
	if err != nil {
		return nil, nil, fmt.Errorf("местный вход не поднят: %w", err)
	}

	return listener, func() {}, nil
}

// listenFailure отличает занятый канал от прочих отказов создания. go-winio v0.6.2 создаёт первый
// дескриптор через `NtCreateNamedPipeFile` с `FILE_CREATE` (`pipe.go:378-408`), и на живое имя ядро
// отвечает `STATUS_OBJECT_NAME_COLLISION`, которую обёртка переводит в `ERROR_ALREADY_EXISTS` —
// это и есть «канал уже поднят кем-то». `ERROR_ACCESS_DENIED` к занятости не относится: при
// `FILE_CREATE` его даёт отказ прав, и назвать его «занят» значило бы соврать о причине.
func listenFailure(name string, failure error) error {
	if errors.Is(failure, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("%w: канал %s", ErrBusy, name)
	}

	return fmt.Errorf("канал не поднят: %w", failure)
}

// CurrentOwner — SID владельца процесса: его разрешает дескриптор безопасности канала, им же
// самопроверка отличает владельца от прочих в ACL каталога состояния.
func CurrentOwner() (string, error) {
	var token windows.Token

	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil {
		return "", fmt.Errorf("токен процесса не открыт: %w", err)
	}

	defer func() { _ = token.Close() }()

	account, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("владелец процесса не прочитан: %w", err)
	}

	return account.User.Sid.String(), nil
}

// randomToken — хвост имени местного входа: случайность, а не номер процесса, потому что номер
// переживает перезагрузку и переиспользуется.
func randomToken() (string, error) {
	buffer := make([]byte, 8)

	_, err := rand.Read(buffer)
	if err != nil {
		return "", fmt.Errorf("имя местного входа не собрано: %w", err)
	}

	return hex.EncodeToString(buffer), nil
}
