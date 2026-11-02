//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/kerbyfc/telegram-bridge/internal/endpoint"
)

// dirAccess отвечает, закрыт ли каталог состояния от других пользователей машины, и говорит, чем
// это видно. На Windows прав у каталога нет: доступ задаёт его DACL, и `os.Stat` её не показывает,
// поэтому она спрашивается у системы и судится `aclVerdict`. Не прочлась — это отказ с причиной:
// закрытость своего каталога самопроверка обязана доказать, а не предположить.
func dirAccess(path string, _ os.FileInfo) (state, string) {
	view, err := readDACL(path)
	if err != nil {
		return stateFailed, "ACL не прочитан: " + err.Error()
	}

	return aclVerdict(view, path)
}

// readDACL читает у системы DACL каталога и SID владельца процесса, которым его записи судятся.
func readDACL(path string) (daclView, error) {
	user, err := endpoint.CurrentOwner()
	if err != nil {
		return daclView{}, fmt.Errorf("SID владельца каталога: %w", err)
	}

	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return daclView{}, fmt.Errorf("дескриптор безопасности %s: %w", path, err)
	}

	// DACL нет в дескрипторе или она пустая ссылка — система открывает объект всем.
	dacl, _, err := descriptor.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && dacl == nil) {
		return daclView{User: user, Null: true, Entries: nil}, nil
	}

	if err != nil {
		return daclView{}, fmt.Errorf("DACL %s: %w", path, err)
	}

	entries := make([]aclEntry, 0, dacl.AceCount)

	for index := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE

		err = windows.GetAce(dacl, index, &ace)
		if err != nil {
			return daclView{}, fmt.Errorf("запись %d DACL %s: %w", index+1, path, err)
		}

		entries = append(entries, aceEntry(ace))
	}

	return daclView{User: user, Null: false, Entries: entries}, nil
}

// aceEntry переводит запись DACL в вид самопроверки. SID у записей разрешения и запрета лежит сразу
// за маской; у прочих видов раскладка своя, и SID из них не читается — их вид называется номером.
func aceEntry(ace *windows.ACCESS_ALLOWED_ACE) aclEntry {
	entry := aclEntry{Kind: aceOther, Type: ace.Header.AceType, SID: "", Name: "", Mask: uint32(ace.Mask)}

	switch ace.Header.AceType {
	case windows.ACCESS_ALLOWED_ACE_TYPE:
		entry.Kind = aceAllow
	case windows.ACCESS_DENIED_ACE_TYPE:
		entry.Kind = aceDeny
	default:
		return entry
	}

	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) //nolint:gosec // раскладку записи задаёт система
	entry.SID = sid.String()
	entry.Name = accountName(sid)

	return entry
}

// accountName — учётная запись SID, как её знает система. Имя — подспорье глазам владельца, а не
// опознание: опознаётся запись по SID, и он называется всегда. Система имени не знает (удалённая
// учётная запись, чужой домен) — пусто, и называется голый SID.
func accountName(sid *windows.SID) string {
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return ""
	}

	if domain == "" {
		return account
	}

	return domain + `\` + account
}
