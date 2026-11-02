package store_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// TestInboxOrderIsBySequence: порядок записей — монотонная последовательность службы, а не имя
// файла: у номера обновления меняется разрядность, и сортировка строк отдавала бы слова
// владельца не в том порядке, в котором он их писал.
func TestInboxOrderIsBySequence(t *testing.T) {
	t.Parallel()

	st := openStore(t)

	for _, seq := range []uint64{9, 10, 100, 1} {
		rec := sample(seq)

		err := rec.Save(st)
		if err != nil {
			t.Fatalf("запись не сохранена: %v", err)
		}
	}

	records, err := store.LoadInbox(st)
	if err != nil {
		t.Fatalf("ящик не прочитан: %v", err)
	}

	got := make([]uint64, 0, len(records))
	for _, rec := range records {
		got = append(got, rec.Seq)
	}

	want := []uint64{1, 9, 10, 100}
	for num := range want {
		if got[num] != want[num] {
			t.Fatalf("порядок записей не по последовательности: %v", got)
		}
	}
}

// TestRecordWithoutAuthorIsRefused: провенанс автора — поле записи, и его отсутствие явная
// ошибка, а не пустая строка.
func TestRecordWithoutAuthorIsRefused(t *testing.T) {
	t.Parallel()

	rec := sample(1)
	rec.Author = protocol.Author{ID: 0, Name: "", Kind: ""}

	err := rec.Validate()
	if !errors.Is(err, store.ErrBadRecord) {
		t.Fatalf("запись без автора принята: %v", err)
	}

	rec = sample(2)
	rec.Provenance = ""

	err = rec.Validate()
	if !errors.Is(err, store.ErrBadRecord) {
		t.Fatalf("запись без провенанса принята: %v", err)
	}
}

// TestRecordSavePanicsWithoutAuthor: нарушенный инвариант записи — паника, а не тихая запись
// слова владельца без того, кто его сказал.
func TestRecordSavePanicsWithoutAuthor(t *testing.T) {
	t.Parallel()

	st := openStore(t)
	rec := sample(1)
	rec.Author = protocol.Author{ID: 0, Name: "", Kind: ""}

	defer func() {
		if recover() == nil {
			t.Fatal("запись без автора сохранена молча")
		}
	}()

	_ = rec.Save(st)
}

// TestJournalSurvivesConcurrentWriters: журнал — дописывание в конец одним писателем, и
// одновременные строки не теряются и не остаются недописанными.
func TestJournalSurvivesConcurrentWriters(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bridge.log")

	jrnl, err := store.OpenJournal(path, 1<<20, 2)
	if err != nil {
		t.Fatalf("журнал не открыт: %v", err)
	}

	const writers = 16

	const lines = 20

	var work sync.WaitGroup

	work.Add(writers)

	for num := range writers {
		go func() {
			defer work.Done()

			for line := range lines {
				_ = jrnl.Write(store.Entry{
					At: time.Now(), Event: "record-accepted", Session: "s1",
					Outcome: "owner-message", Code: "", Text: strings.Repeat("я", num+line),
					TopicID: 1,
				})
			}
		}()
	}

	work.Wait()

	err = jrnl.Close()
	if err != nil {
		t.Fatalf("журнал не закрыт: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("журнал не прочитан: %v", err)
	}

	raw := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(raw) != writers*lines {
		t.Fatalf("журнал потерял строки: %d из %d", len(raw), writers*lines)
	}

	for _, line := range raw {
		var entry store.Entry

		err = json.Unmarshal([]byte(line), &entry)
		if err != nil {
			t.Fatalf("строка журнала недописана: %q", line)
		}
	}
}

// TestStatePermissions: каталоги состояния 0700, файлы 0600 — состояние моста не читается любым
// пользователем машины.
func TestStatePermissions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	st, err := store.Open(root)
	if err != nil {
		t.Fatalf("состояние не открыто: %v", err)
	}

	err = (&store.Updates{Offset: 7}).Save(st)
	if err != nil {
		t.Fatalf("смещение не сохранено: %v", err)
	}

	dir, err := os.Stat(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatalf("каталог сессий не найден: %v", err)
	}

	if dir.Mode().Perm() != store.DirPerm {
		t.Fatalf("права каталога состояния: %v", dir.Mode().Perm())
	}

	file, err := os.Stat(filepath.Join(root, "updates.json"))
	if err != nil {
		t.Fatalf("файл смещения не найден: %v", err)
	}

	if file.Mode().Perm() != store.FilePerm {
		t.Fatalf("права файла состояния: %v", file.Mode().Perm())
	}
}

// TestTopicsRegistryIsDirectBothWays: реестр тем ищется прямым словарём в обе стороны, и запись
// пропавшей темы убирается целиком.
func TestTopicsRegistryIsDirectBothWays(t *testing.T) {
	t.Parallel()

	st := openStore(t)

	topics, err := store.LoadTopics(st)
	if err != nil {
		t.Fatalf("реестр не прочитан: %v", err)
	}

	topics.Put(store.Topic{ID: 42, Name: "alpha", InMode: false, PinnedID: 7, IconUnknown: false})
	topics.Rename(42, "beta")

	if _, ok := topics.ByNameLookup("alpha"); ok {
		t.Fatal("прежнее имя осталось в реестре")
	}

	entry, ok := topics.ByIDLookup(42)
	if !ok || entry.Name != "beta" || entry.PinnedID != 7 {
		t.Fatalf("запись после переименования не та: %+v", entry)
	}

	topics.Drop(42)

	if _, ok := topics.ByNameLookup("beta"); ok {
		t.Fatal("запись пропавшей темы осталась в реестре")
	}
}

// TestPutRefusesForeignName: имя, занятое другой темой, затереть нельзя. Затёртая запись исчезла
// бы из реестра при живой теме, а её id продолжал бы смотреть на отданное имя — и поиск по id
// начал бы отдавать одну тему вместо другой. Восстановить запись неоткуда: списка тем Telegram
// не отдаёт, поэтому это паника, а не тихая порча.
func TestPutRefusesForeignName(t *testing.T) {
	t.Parallel()

	topics := &store.Topics{ByName: map[string]store.Topic{}, ByID: map[string]string{}}
	topics.Put(store.Topic{ID: 1, Name: "alpha", InMode: false, PinnedID: 0, IconUnknown: false})

	defer func() {
		if recover() == nil {
			t.Fatal("запись чужой темы затёрта молча")
		}
	}()

	topics.Put(store.Topic{ID: 2, Name: "alpha", InMode: false, PinnedID: 0, IconUnknown: false})
}

// TestFreeNameSeesOwnRecord: своё же имя занятым не считается, чужое — считается.
func TestFreeNameSeesOwnRecord(t *testing.T) {
	t.Parallel()

	topics := &store.Topics{ByName: map[string]store.Topic{}, ByID: map[string]string{}}
	topics.Put(store.Topic{ID: 1, Name: "alpha", InMode: false, PinnedID: 0, IconUnknown: false})

	if !topics.FreeName("alpha", 1) {
		t.Fatal("своё имя сочтено занятым")
	}

	if topics.FreeName("alpha", 2) {
		t.Fatal("чужое имя сочтено свободным")
	}

	if !topics.FreeName("alpha#2", 2) {
		t.Fatal("свободное имя сочтено занятым")
	}
}

// TestClosedTopicFreesNameAndKeepsRecord: закрытая тема имени не держит — новая запись того же имени
// не падает, — а по id находится по-прежнему и правится на месте. Запись переживает файл состояния и
// возвращается в реестр имён под названным именем.
func TestClosedTopicFreesNameAndKeepsRecord(t *testing.T) {
	t.Parallel()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("состояние не открыто: %v", err)
	}

	topics := &store.Topics{ByName: map[string]store.Topic{}, ByID: map[string]string{}, Closed: nil}
	topics.Put(store.Topic{ID: 1, Name: "alpha", InMode: true, PinnedID: 5, IconUnknown: false})
	topics.Close(1)

	if !topics.IsClosed(1) || !topics.FreeName("alpha", 2) {
		t.Fatal("закрытая тема держит имя")
	}

	topics.Put(store.Topic{ID: 2, Name: "alpha", InMode: false, PinnedID: 0, IconUnknown: false})

	entry, ok := topics.ByIDLookup(1)
	if !ok || entry.Name != "alpha" || entry.PinnedID != 5 {
		t.Fatalf("закрытая тема по id: %+v", entry)
	}

	entry.PinnedID = 6
	topics.Put(entry)

	if held, _ := topics.ByNameLookup("alpha"); held.ID != 2 {
		t.Fatalf("правка закрытой записи задела реестр имён: %+v", held)
	}

	err = topics.Save(st)
	if err != nil {
		t.Fatalf("реестр не записан: %v", err)
	}

	loaded, err := store.LoadTopics(st)
	if err != nil {
		t.Fatalf("реестр не прочитан: %v", err)
	}

	loaded.Reopen(1, "alpha#2")

	if loaded.IsClosed(1) {
		t.Fatal("открытая снова тема числится закрытой")
	}

	held, ok := loaded.ByNameLookup("alpha#2")
	if !ok || held.ID != 1 || held.PinnedID != 6 {
		t.Fatalf("открытая снова тема: %+v", held)
	}

	loaded.Close(1)
	loaded.Drop(1)

	if _, ok = loaded.ByIDLookup(1); ok {
		t.Fatal("удалённая закрытая тема осталась в реестре")
	}
}

// TestWriteAtomicFailureLeavesTargetAndNoTemp: сорвавшаяся замена файла состояния прежнего
// содержимого не трогает и временного файла после себя не оставляет: половинчатой записи, которую
// следующий старт прочёл бы как состояние, не бывает. Замена срывается на последнем шаге — цель
// занята каталогом, и переименование отказывает, когда временный файл уже записан.
func TestWriteAtomicFailureLeavesTargetAndNoTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "updates.json")
	inside := filepath.Join(target, "прежнее")

	err := os.Mkdir(target, store.DirPerm)
	if err != nil {
		t.Fatalf("цель не занята каталогом: %v", err)
	}

	err = os.WriteFile(inside, []byte("прежнее содержимое"), store.FilePerm)
	if err != nil {
		t.Fatalf("прежнее содержимое не записано: %v", err)
	}

	err = store.WriteAtomic(target, []byte(`{"offset": 9}`))
	if err == nil {
		t.Fatal("замена поверх занятой цели прошла молча")
	}

	body, err := os.ReadFile(inside)
	if err != nil || string(body) != "прежнее содержимое" {
		t.Fatalf("сорвавшаяся замена тронула прежнее содержимое: %q, %v", body, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("каталог не прочитан: %v", err)
	}

	if len(entries) != 1 || entries[0].Name() != "updates.json" {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}

		t.Fatalf("после сорвавшейся замены в каталоге остался мусор: %v", names)
	}
}

// openStore готовит корень состояния в своём каталоге.
func openStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("состояние не открыто: %v", err)
	}

	return st
}

// sample готовит годную запись ящика.
func sample(seq uint64) *store.Record {
	return &store.Record{
		Seq:        seq,
		Session:    "s1",
		ID:         store.NewID("d", seq),
		Text:       "слово владельца",
		Author:     protocol.Author{ID: 1, Name: "Александр", Kind: protocol.AuthorHuman},
		Provenance: "Доставлено из Telegram, автор Александр, человек",
		MessageID:  5,
		TopicID:    42,
		TopicName:  "alpha",
		At:         time.Now(),
		Delivery:   protocol.DeliveryOwnerMessage,
	}
}
