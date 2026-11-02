package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// TestJournalTurnsOverBySize: журнал меняет поколение по размеру и держит не больше названного
// числа прежних. Без ротации он растёт без предела, а место на диске у владельца кончается молча.
func TestJournalTurnsOverBySize(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bridge.log")

	jrnl, err := store.OpenJournal(path, 200, 2)
	if err != nil {
		t.Fatalf("журнал не открыт: %v", err)
	}

	t.Cleanup(func() { _ = jrnl.Close() })

	for num := range 40 {
		err = jrnl.Write(store.Entry{
			At: time.Now(), Event: "poll-failed", Session: "", Outcome: "",
			Code: "network", Text: strings.Repeat("я", num%7), TopicID: 0,
		})
		if err != nil {
			t.Fatalf("строка журнала не записана: %v", err)
		}
	}

	for _, name := range []string{path, path + ".1", path + ".2"} {
		info, statErr := os.Stat(name)
		if statErr != nil {
			t.Fatalf("поколения %s нет: %v", name, statErr)
		}

		if info.Size() == 0 {
			t.Fatalf("поколение %s пусто", name)
		}
	}

	_, err = os.Stat(path + ".3")
	if !os.IsNotExist(err) {
		t.Fatalf("журнал держит поколений больше названного: %v", err)
	}
}

// TestJournalReopenKeepsLineAfterTornTail: журнал, кончившийся недописанной строкой (обрыв записи,
// полный диск), после перезапуска службы не склеивает с ней первую новую строку. Склейка не
// разбирается ни самопроверкой, ни глазами, и пропадала бы как раз строка о том, что было после
// отказа. Сам обрывок остаётся на месте своей строкой: след отказа не стирается.
//
//nolint:cyclop // один путь журнала от обрыва до чтения хвоста: каждый шаг — своя проверка
func TestJournalReopenKeepsLineAfterTornTail(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bridge.log")
	torn := `{"at":"обрыв`
	whole := `{"at":"2026-10-31T10:00:00Z","event":"poll-failed","code":"network"}` + "\n"

	err := os.WriteFile(path, []byte(whole+torn), store.FilePerm)
	if err != nil {
		t.Fatalf("журнал с обрывом не записан: %v", err)
	}

	jrnl, err := store.OpenJournal(path, 1<<20, 2)
	if err != nil {
		t.Fatalf("журнал не открыт: %v", err)
	}

	err = jrnl.Write(store.Entry{
		At: time.Now(), Event: "record-queued", Session: "s1", Outcome: "owner-message",
		Code: "", Text: "d-000001-ab", TopicID: 1,
	})
	if err != nil {
		t.Fatalf("строка журнала не записана: %v", err)
	}

	err = jrnl.Close()
	if err != nil {
		t.Fatalf("журнал не закрыт: %v", err)
	}

	entries, err := store.TailJournal(path, 5)
	if err != nil {
		t.Fatalf("хвост журнала не прочитан: %v", err)
	}

	if len(entries) != 2 || entries[0].Event != "poll-failed" || entries[1].Event != "record-queued" {
		t.Fatalf("первая строка после обрыва склеилась с ним и пропала: %+v", entries)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("журнал не прочитан: %v", err)
	}

	if !strings.Contains(string(body), "\n"+torn+"\n") {
		t.Fatalf("обрывок не остался своей строкой: %q", body)
	}
}

// TestJournalReopenAfterWholeLineAddsNothing: журнал, кончившийся целой строкой, открытие не
// трогает: перевод строки дописывается только за обрывком, и пустых строк от перезапусков службы
// в журнале не копится.
func TestJournalReopenAfterWholeLineAddsNothing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bridge.log")
	whole := `{"at":"2026-10-31T10:00:00Z","event":"record-queued","code":""}` + "\n"

	err := os.WriteFile(path, []byte(whole), store.FilePerm)
	if err != nil {
		t.Fatalf("журнал не записан: %v", err)
	}

	jrnl, err := store.OpenJournal(path, 1<<20, 2)
	if err != nil {
		t.Fatalf("журнал не открыт: %v", err)
	}

	err = jrnl.Close()
	if err != nil {
		t.Fatalf("журнал не закрыт: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("журнал не прочитан: %v", err)
	}

	if string(body) != whole {
		t.Fatalf("открытие тронуло журнал с целой последней строкой: %q", body)
	}
}

// TestJournalRefusesBadRotation: негодные значения ротации — отказ, а не тихая подмена своими:
// журнал без предела растёт без предела, и решать это за владельца молча нельзя.
func TestJournalRefusesBadRotation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bridge.log")

	_, err := store.OpenJournal(path, 0, 3)
	if err == nil {
		t.Fatal("журнал открылся без предела поколения")
	}

	_, err = store.OpenJournal(path, 1<<20, 0)
	if err == nil {
		t.Fatal("журнал открылся без хранимых поколений")
	}
}

// TestTailJournalReadsLastLines: самопроверка читает последние строки журнала — ими владелец
// видит причину падения, когда службы уже нет и спросить её нечем. Недописанная строка разбор не
// валит: она значит обрыв записи, а не конец журнала.
func TestTailJournalReadsLastLines(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bridge.log")

	jrnl, err := store.OpenJournal(path, 1<<20, 2)
	if err != nil {
		t.Fatalf("журнал не открыт: %v", err)
	}

	for _, code := range []string{"network", "tls-untrusted", "version"} {
		err = jrnl.Write(store.Entry{
			At: time.Now(), Event: "poll-failed", Session: "", Outcome: "",
			Code: code, Text: "отказ " + code, TopicID: 0,
		})
		if err != nil {
			t.Fatalf("строка журнала не записана: %v", err)
		}
	}

	err = jrnl.Close()
	if err != nil {
		t.Fatalf("журнал не закрыт: %v", err)
	}

	broken, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, store.FilePerm)
	if err != nil {
		t.Fatalf("журнал не открыт на дозапись: %v", err)
	}

	_, err = broken.WriteString(`{"at":"обрыв`)
	if err != nil {
		t.Fatalf("недописанная строка не добавлена: %v", err)
	}

	_ = broken.Close()

	entries, err := store.TailJournal(path, 2)
	if err != nil {
		t.Fatalf("хвост журнала не прочитан: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("строк хвоста %d, а просили 2: %+v", len(entries), entries)
	}

	if entries[1].Code != "version" {
		t.Fatalf("последняя строка хвоста не та: %+v", entries[1])
	}
}

// TestTailLinesReadsPlainText: вывод самого процесса службы — не журнал: его пишет система
// автозапуска, строки там текст как есть, и паника лежит именно в нём. Самопроверка читает хвост
// этого файла, поэтому разбор JSON здесь не при чём.
func TestTailLinesReadsPlainText(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "launchd.log")
	body := "panic: состояние службы не записано\n\n\tgoroutine 1\n\tcore.mustSave\n"

	err := os.WriteFile(path, []byte(body), store.FilePerm)
	if err != nil {
		t.Fatalf("вывод процесса не записан: %v", err)
	}

	lines, err := store.TailLines(path, 2)
	if err != nil {
		t.Fatalf("хвост вывода не прочитан: %v", err)
	}

	if len(lines) != 2 || lines[1] != "core.mustSave" {
		t.Fatalf("хвост вывода не тот: %+v", lines)
	}

	// Файла нет — строк нет, и это не ошибка: служба под автозапуском могла не подниматься.
	lines, err = store.TailLines(filepath.Join(t.TempDir(), "нет.log"), 2)
	if err != nil || lines != nil {
		t.Fatalf("отсутствие вывода процесса названо отказом: %+v, %v", lines, err)
	}
}

// TestTailJournalWithoutFile: журнала нет — строк нет, и это не ошибка: служба могла ни разу не
// подниматься, и самопроверка обязана сказать об этом, а не упасть.
func TestTailJournalWithoutFile(t *testing.T) {
	t.Parallel()

	entries, err := store.TailJournal(filepath.Join(t.TempDir(), "нет.log"), 3)
	if err != nil {
		t.Fatalf("отсутствие журнала названо отказом: %v", err)
	}

	if entries != nil {
		t.Fatalf("у несуществующего журнала нашлись строки: %+v", entries)
	}
}
