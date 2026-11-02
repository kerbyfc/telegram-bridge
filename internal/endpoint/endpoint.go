// Package endpoint — адрес границы службы и её транспорт. На macOS граница — Unix-сокет: путь в
// каталоге состояния, каталог 0700, сокет 0600. На Windows сокета нет, и тот же протокол кадров
// едет именованным каналом; имя канала берётся из того же ключа настроек, что путь сокета.
//
// Служба и соединение сессии о площадке не знают: они называют адрес и получают слушателя или
// соединение. Чем адрес открыт, решает этот пакет, поэтому кадры и поведение вытеснения
// соединений остаются одним кодом на обеих площадках.
package endpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	// pipePrefix — начало полного имени именованного канала Windows.
	pipePrefix = `\\.\pipe\`
	// defaultNameLength — сколько знаков отпечатка идёт в имя канала по умолчанию. Шестнадцать
	// знаков шестнадцатеричной записи — это 64 бита: два корня состояния сойтись в имени не могут.
	defaultNameLength = 16
)

// ErrBusy — адрес занят живой службой: второй процесс на машину не поднимается.
var ErrBusy = errors.New("адрес занят живой службой")

// ErrName — имя канала негодно: путь в ключе `socket_path` значит, что настройки перенесены с
// macOS без правки, а канал носит имя, а не путь.
var ErrName = errors.New("имя канала негодно")

// pipeName собирает полное имя канала из значения ключа `socket_path`: полное имя остаётся собой,
// короткое получает начало. Путь — отказ: на Windows этот ключ значит имя канала, и принятый за
// имя путь открылся бы каналом со странным именем, а не сказал бы, что настройки не те.
func pipeName(configured string) (string, error) {
	if configured == "" {
		return "", fmt.Errorf("%w: пустое имя", ErrName)
	}

	name := strings.TrimPrefix(configured, pipePrefix)

	if strings.ContainsAny(name, `/\:`) {
		return "", fmt.Errorf(
			"%w: %q — путь, а канал носит имя: назови имя или полное `%sимя`",
			ErrName, configured, pipePrefix,
		)
	}

	return pipePrefix + name, nil
}

// defaultPipeName — имя канала по умолчанию: оно выводится из корня состояния, поэтому у службы
// ветки со своим корнем свой канал, и две службы на машине друг друга не вытесняют. Имя не
// угадывается владельцем, а называется установкой и самопроверкой.
func defaultPipeName(stateRoot string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateRoot)))

	return pipePrefix + "dm-telegram-bridge-" + hex.EncodeToString(sum[:])[:defaultNameLength]
}

// pipeSecurityDescriptor — SDDL канала: своя запись доступа без наследования, и в ней только
// владелец с полными правами. Плата за явность — канал не открывает никто, кроме владельца, и
// наследованные записи профиля (администраторы машины, SYSTEM) к нему не применяются.
func pipeSecurityDescriptor(owner string) (string, error) {
	if owner == "" {
		return "", fmt.Errorf("%w: владелец канала неизвестен", ErrName)
	}

	if !strings.HasPrefix(owner, "S-") || strings.ContainsAny(owner, `()\`) {
		return "", fmt.Errorf("%w: владелец канала %q — не SID", ErrName, owner)
	}

	return "D:P(A;;GA;;;" + owner + ")", nil
}
