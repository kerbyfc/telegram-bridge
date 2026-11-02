package cli

import (
	"fmt"
	"slices"
	"strings"
)

// aceKind — вид записи DACL, как её судит самопроверка.
type aceKind int

// Виды записи: разрешение, запрет и вид, которого самопроверка не разбирает.
const (
	// aceAllow открывает доступ тому, кому названа.
	aceAllow aceKind = iota
	// aceDeny доступ запрещает: закрытости каталога она не убавляет.
	aceDeny
	// aceOther — вид с иной раскладкой: кому он что открывает, самопроверка не читает.
	aceOther
)

// aclEntry — одна запись DACL каталога состояния.
type aclEntry struct {
	Kind aceKind
	// Type — номер вида у системы: им называется вид, которого самопроверка не разбирает.
	Type uint8
	SID  string
	// Name — учётная запись SID, как её назвала система; пусто — система её не знает, и
	// называется голый SID.
	Name string
	Mask uint32
}

// daclView — DACL каталога состояния, как её отдала система.
type daclView struct {
	// User — SID владельца процесса: каталог состояния — его.
	User string
	// Null — DACL нет вовсе, и система открывает каталог всем.
	Null    bool
	Entries []aclEntry
}

// trustedSIDs — кроме владельца процесса, кому каталог открыт по праву, а не по ошибке. SYSTEM и
// администраторы машины на Windows — то же, что root на macOS: прочесть они могут что угодно и без
// записи в ACL, и права 0700 на macOS им тоже ничего не закрывают. CREATOR OWNER и OWNER RIGHTS
// означают самого владельца.
var trustedSIDs = map[string]bool{
	"S-1-5-18":     true, // SYSTEM
	"S-1-5-32-544": true, // BUILTIN\Administrators
	"S-1-3-0":      true, // CREATOR OWNER
	"S-1-3-4":      true, // OWNER RIGHTS
}

// who называет того, кому запись открывает доступ: учётной записью и SID, а без имени — SID.
func (e aclEntry) who() string {
	if e.Name == "" {
		return e.SID
	}

	return e.Name + " (" + e.SID + ")"
}

// aclVerdict судит DACL каталога состояния: закрыт ли он от других пользователей машины. Открыт —
// если хоть одна запись разрешения что-то даёт кому-то, кроме владельца и `trustedSIDs`, или DACL
// нет вовсе. Записи, наследуемые только вложенным, судятся наравне с прочими: вложенное — это и
// есть файлы состояния. Запрет закрытости не убавляет, и сверять, перекрывает ли он чьё-то
// разрешение, самопроверка не берётся: запись, открывающая каталог, называется в любом случае.
//
// Вид записи, которого самопроверка не разбирает, — «не подтверждено» с названным видом, а не
// «закрыт»: кому он что открывает, неизвестно. Каталог, открытость которого уже видна по другим
// записям, это не спасает — он открыт.
func aclVerdict(view daclView, path string) (state, string) {
	if view.Null {
		return stateFailed, "ACL: DACL нет вовсе, и каталог открыт всем"
	}

	open, unread := sortEntries(view)

	switch {
	case len(open) > 0:
		return stateFailed, "ACL открывает каталог: " + strings.Join(open, ", ")
	case len(unread) > 0:
		return stateUnknown, "ACL: записи (" + strings.Join(unread, ", ") +
			") самопроверка не разбирает — проверь `icacls " + path + "`"
	default:
		return stateOK, "ACL: только владелец, SYSTEM и администраторы машины"
	}
}

// sortEntries разбирает записи DACL: кому они открывают каталог сверх владельца и `trustedSIDs` и
// какие виды записей самопроверка не прочла. Каждый назван раз, сколько бы записей его ни несло.
func sortEntries(view daclView) ([]string, []string) {
	var open, unread []string

	for _, entry := range view.Entries {
		switch entry.Kind {
		case aceDeny:
		case aceOther:
			unread = addOnce(unread, fmt.Sprintf("вид %d", entry.Type))
		case aceAllow:
			if entry.Mask == 0 || entry.SID == view.User || trustedSIDs[entry.SID] {
				continue
			}

			open = addOnce(open, entry.who())
		default:
			panic(fmt.Sprintf("вид записи DACL вне перечня: %d", entry.Kind))
		}
	}

	return open, unread
}

// addOnce дописывает строку в список, если её там ещё нет.
func addOnce(list []string, item string) []string {
	if slices.Contains(list, item) {
		return list
	}

	return append(list, item)
}
