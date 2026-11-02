package install

import (
	"testing"
	"time"
)

// TestBuildFreshSecondPrecision: время старта службы приходит по протоколу секундами, а время файла —
// с наносекундами. Сравнение в разных точностях отказывало бы на удачном перезапуске, поднятом в ту
// же секунду, что записан бинарник, поэтому время файла приводится к секунде.
func TestBuildFreshSecondPrecision(t *testing.T) {
	t.Parallel()

	written := time.Date(2026, 10, 27, 12, 0, 0, 300_000_000, time.UTC)

	cases := []struct {
		name    string
		started time.Time
		want    bool
	}{
		{
			name:    "та же секунда",
			started: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
			want:    true,
		},
		{
			name:    "следующая секунда",
			started: time.Date(2026, 10, 27, 12, 0, 1, 0, time.UTC),
			want:    true,
		},
		{
			name:    "секундой раньше",
			started: time.Date(2026, 10, 27, 11, 59, 59, 0, time.UTC),
			want:    false,
		},
	}

	for _, one := range cases {
		if got := buildFresh(one.started, written); got != one.want {
			t.Fatalf("%s: сборка свежая=%v, ждали %v (старт %s, файл %s)",
				one.name, got, one.want, one.started.Format(time.RFC3339Nano),
				written.Format(time.RFC3339Nano))
		}
	}
}
