//go:build windows

package cli //nolint:testpackage // чтение DACL у системы неэкспортировано: проверяется на живом каталоге

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/kerbyfc/telegram-bridge/internal/endpoint"
)

// TestDirAccessReadsDACL: самопроверка спрашивает DACL каталога у системы, а не отвечает «не
// подтверждено»: каталог только владельца проверку проходит, а открытый всем на чтение — валит её
// и называет, кому он открыт.
func TestDirAccessReadsDACL(t *testing.T) {
	t.Parallel()

	owner, err := endpoint.CurrentOwner()
	if err != nil {
		t.Fatalf("владелец процесса не прочитан: %v", err)
	}

	cases := map[string]struct {
		sddl string
		want state
	}{
		"только владелец": {sddl: "D:P(A;OICI;FA;;;" + owner + ")", want: stateOK},
		"и все на чтение": {sddl: "D:P(A;OICI;FA;;;" + owner + ")(A;OICI;FR;;;WD)", want: stateFailed},
	}

	for name, one := range cases {
		dir := t.TempDir()
		setDACL(t, dir, one.sddl)

		got, detail := dirAccess(dir, nil)
		if got != one.want {
			t.Fatalf("%s: исход %s, а ждали %s: %q", name, got, one.want, detail)
		}

		if one.want == stateFailed && !strings.Contains(detail, "S-1-1-0") {
			t.Fatalf("%s: открытый каталог не назвал, кому открыт: %q", name, detail)
		}
	}
}

// setDACL ставит каталогу DACL из SDDL без наследования от родителя.
func setDACL(t *testing.T, path, sddl string) {
	t.Helper()

	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SDDL %q не разобран: %v", sddl, err)
	}

	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("DACL из SDDL %q не взята: %v", sddl, err)
	}

	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		t.Fatalf("DACL каталога %s не поставлена: %v", path, err)
	}
}
