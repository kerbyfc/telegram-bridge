package cli //nolint:testpackage // суд над DACL неэкспортирован: проверяется без Windows, по записям

import (
	"strings"
	"testing"
)

// SID стенда: владелец процесса, чужой пользователь машины и общеизвестные группы.
const (
	ownerSID      = "S-1-5-21-1004336348-1177238915-682003330-1001"
	strangerSID   = "S-1-5-21-1004336348-1177238915-682003330-1002"
	everyoneSID   = "S-1-1-0"
	usersSID      = "S-1-5-32-545"
	systemSID     = "S-1-5-18"
	adminsSID     = "S-1-5-32-544"
	creatorSID    = "S-1-3-0"
	fullAccess    = 0x1f01ff
	readExecute   = 0x1200a9
	stateRootPath = `C:\Users\owner\.dm-telegram-go`
)

// profileEntries — DACL каталога, унаследованная от профиля пользователя: владелец, SYSTEM,
// администраторы машины и CREATOR OWNER для вложенного. Так выглядит закрытый каталог по умолчанию.
func profileEntries() []aclEntry {
	return []aclEntry{
		allow(systemSID, "NT AUTHORITY\\SYSTEM", fullAccess),
		allow(adminsSID, "BUILTIN\\Administrators", fullAccess),
		allow(ownerSID, "MACHINE\\owner", fullAccess),
		allow(creatorSID, "CREATOR OWNER", fullAccess),
	}
}

// allow — запись разрешения.
func allow(sid, name string, mask uint32) aclEntry {
	return aclEntry{Kind: aceAllow, Type: 0, SID: sid, Name: name, Mask: mask}
}

// TestACLClosedProfileDirIsOK: каталог, закрытый от других пользователей машины, проверку
// проходит — и «не подтверждено» в ответе больше нет.
func TestACLClosedProfileDirIsOK(t *testing.T) {
	t.Parallel()

	verdict, detail := aclVerdict(daclView{User: ownerSID, Null: false, Entries: profileEntries()}, stateRootPath)
	if verdict != stateOK {
		t.Fatalf("закрытый каталог не прошёл: %s, %q", verdict, detail)
	}
}

// TestACLOpenDirFailsByName: запись, открывающая каталог кому-то ещё, валит проверку так же, как
// права шире 0700 на macOS, и называет, кому именно он открыт. Запись повторённая — называется раз.
func TestACLOpenDirFailsByName(t *testing.T) {
	t.Parallel()

	cases := map[string]aclEntry{
		"все":                allow(everyoneSID, "Everyone", readExecute),
		"пользователи":       allow(usersSID, "BUILTIN\\Users", readExecute),
		"чужой пользователь": allow(strangerSID, "", fullAccess),
	}

	for name, entry := range cases {
		entries := append(profileEntries(), entry, entry)

		verdict, detail := aclVerdict(daclView{User: ownerSID, Null: false, Entries: entries}, stateRootPath)
		if verdict != stateFailed || !strings.Contains(detail, entry.SID) {
			t.Fatalf("%s: открытый каталог не назван: %s, %q", name, verdict, detail)
		}

		if strings.Count(detail, entry.SID) != 1 {
			t.Fatalf("%s: повторённая запись названа дважды: %q", name, detail)
		}
	}
}

// TestACLWithoutDACLFails: DACL нет вовсе — система открывает каталог всем.
func TestACLWithoutDACLFails(t *testing.T) {
	t.Parallel()

	verdict, detail := aclVerdict(daclView{User: ownerSID, Null: true, Entries: nil}, stateRootPath)
	if verdict != stateFailed || !strings.Contains(detail, "открыт всем") {
		t.Fatalf("каталог без DACL не назван открытым: %s, %q", verdict, detail)
	}
}

// TestACLDenyAndEmptyMaskKeepClosed: запрет закрытости не убавляет, а разрешение без прав ничего не
// открывает.
func TestACLDenyAndEmptyMaskKeepClosed(t *testing.T) {
	t.Parallel()

	entries := append(profileEntries(),
		aclEntry{Kind: aceDeny, Type: 1, SID: everyoneSID, Name: "Everyone", Mask: fullAccess},
		allow(usersSID, "BUILTIN\\Users", 0),
	)

	verdict, detail := aclVerdict(daclView{User: ownerSID, Null: false, Entries: entries}, stateRootPath)
	if verdict != stateOK {
		t.Fatalf("запрет или пустое разрешение открыли каталог: %s, %q", verdict, detail)
	}
}

// TestACLUnreadEntryIsUnknown: запись вида, которого самопроверка не разбирает, — «не подтверждено»
// с названным видом и командой системы, а не «закрыт». Каталог, уже открытый по другим записям, она
// не спасает.
func TestACLUnreadEntryIsUnknown(t *testing.T) {
	t.Parallel()

	odd := aclEntry{Kind: aceOther, Type: 5, SID: "", Name: "", Mask: fullAccess}
	branchRoot := `D:\dm\06-sluzhba\state`

	// Команда системы называет тот каталог, о котором спросили, а не каталог по умолчанию.
	verdict, detail := aclVerdict(
		daclView{User: ownerSID, Null: false, Entries: append(profileEntries(), odd)}, branchRoot)
	if verdict != stateUnknown || !strings.Contains(detail, "вид 5") ||
		!strings.Contains(detail, "`icacls "+branchRoot+"`") {
		t.Fatalf("неразобранная запись не названа: %s, %q", verdict, detail)
	}

	verdict, detail = aclVerdict(daclView{
		User: ownerSID, Null: false,
		Entries: append(profileEntries(), odd, allow(everyoneSID, "Everyone", readExecute)),
	}, stateRootPath)
	if verdict != stateFailed {
		t.Fatalf("неразобранная запись спрятала открытый каталог: %s, %q", verdict, detail)
	}
}
