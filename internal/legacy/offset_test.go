package legacy_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/legacy"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// TestReadOffsetReadsWhatOldBridgeWrites: смещение старого моста — голое число, как он его пишет;
// пробелы по краям терпит и его собственное чтение. Файла нет — смещения нет, и это не отказ.
func TestReadOffsetReadsWhatOldBridgeWrites(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"709112834", " 709112834\n"} {
		got, err := legacy.ReadOffset(offsetFile(t, body))
		if err != nil || got != 709112834 {
			t.Fatalf("смещение %q прочитано как %d (%v)", body, got, err)
		}
	}

	got, err := legacy.ReadOffset(filepath.Join(t.TempDir(), legacy.OffsetFile))
	if err != nil || got != 0 {
		t.Fatalf("нет файла смещения — это не отказ и не смещение: %d, %v", got, err)
	}
}

// TestReadOffsetRefusesBrokenOffset: битое смещение — отказ с причиной, а не нуль: нуль значил бы
// «смещения нет», и служба приняла бы второй раз всё, что Telegram ещё не подтвердил.
func TestReadOffsetRefusesBrokenOffset(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"пусто":         "",
		"не число":      "сто",
		"дробь":         "12.5",
		"нуль":          "0",
		"отрицательное": "-5",
		"не влезает":    "99999999999999999999",
	} {
		_, err := legacy.ReadOffset(offsetFile(t, body))
		if !errors.Is(err, legacy.ErrBadState) {
			t.Fatalf("%s: битое смещение %q не отбито: %v", name, body, err)
		}
	}
}

// TestPortOffsetNeverGoesBack: смещение старого моста ложится в состояние службы, если оно дальше
// её собственного, а назад смещение не идёт — откат вернул бы службе разобранные слова второй раз.
func TestPortOffsetNeverGoesBack(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		old, service, want int64
		outcome            legacy.OffsetOutcome
	}{
		"смещения нет":         {old: 0, service: 0, want: 0, outcome: legacy.OffsetNone},
		"смещения нет, служба": {old: 0, service: 50, want: 50, outcome: legacy.OffsetNone},
		"служба не опрашивала": {old: 100, service: 0, want: 100, outcome: legacy.OffsetPlaced},
		"служба отстала":       {old: 100, service: 40, want: 100, outcome: legacy.OffsetPlaced},
		"служба там же":        {old: 100, service: 100, want: 100, outcome: legacy.OffsetKept},
		"служба ушла дальше":   {old: 100, service: 140, want: 140, outcome: legacy.OffsetKept},
	}

	for name, one := range cases {
		updates := &store.Updates{Offset: one.service}

		outcome := legacy.PortOffset(one.old, updates)
		if outcome != one.outcome || updates.Offset != one.want {
			t.Fatalf("%s: исход %s, смещение %d, а ждали %s и %d", name, outcome, updates.Offset, one.outcome, one.want)
		}
	}
}

// offsetFile кладёт файл смещения старого моста с этим содержимым.
func offsetFile(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), legacy.OffsetFile)

	err := os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatalf("смещение старого моста не записано: %v", err)
	}

	return path
}
