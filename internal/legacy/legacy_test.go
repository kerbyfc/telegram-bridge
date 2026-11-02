package legacy_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/legacy"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// TestPortPutsTopics: темы старого реестра ложатся в новый обеими сторонами словаря — имя ищется
// по имени, а обратный поиск по id отвечает тем же именем. Закреплённой строки у перенесённой темы
// нет, а значок неизвестен: старый мост их не хранил, и выдумывать их значит утверждать о теме то,
// чего никто не видел.
func TestPortPutsTopics(t *testing.T) {
	t.Parallel()

	st := openStore(t)
	topics := load(t, st)

	report := read(t, `{"мост": 101, "шестой-этап": 202}`).Port(topics)

	if report.Read != 2 || report.Placed != 2 || len(report.Skipped) != 0 {
		t.Fatalf("перенос не назвал своего исхода: %+v", report)
	}

	save(t, st, topics)

	topics = load(t, st)

	for name, id := range map[string]int64{"мост": 101, "шестой-этап": 202} {
		assertTopic(t, topics, name, id)
	}
}

// TestPortCutsLongName: имя длиннее предела Telegram переносится резаным — тем же правилом, что и в
// остальном реестре, — и по резаному же имени второй перенос узнаёт тему на месте, а не называет её
// «уже в реестре под другим именем».
func TestPortCutsLongName(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("я", 200)
	cut := botapi.TopicName(long, "")

	st := openStore(t)
	topics := load(t, st)
	registry := read(t, `{"`+long+`": 101}`)

	report := registry.Port(topics)
	if report.Placed != 1 {
		t.Fatalf("длинное имя не перенесено: %+v", report)
	}

	assertTopic(t, topics, cut, 101)

	if _, uncut := topics.ByNameLookup(long); uncut {
		t.Fatal("длинное имя перенесено нерезаным")
	}

	again := registry.Port(topics)
	if again.Placed != 0 || len(again.Skipped) != 1 || again.Skipped[0].Reason != legacy.ReasonInPlace {
		t.Fatalf("второй перенос не узнал тему на месте: %+v", again)
	}
}

// TestPortNamesCutClash: два длинных имени, совпавшие после резки, — вторая тема пропускается, и
// причина называет совпадение с соседней темой того же реестра, а не «привязку новее старого
// реестра»: имя заняла не привязка, а тема этого же запуска.
func TestPortNamesCutClash(t *testing.T) {
	t.Parallel()

	one := strings.Repeat("я", 200) + "а"
	two := strings.Repeat("я", 200) + "б"

	report := read(t, `{"`+one+`": 101, "`+two+`": 102}`).Port(load(t, openStore(t)))

	if report.Placed != 1 || len(report.Skipped) != 1 {
		t.Fatalf("совпавшие после резки имена перенесены не так: %+v", report)
	}

	if skip := report.Skipped[0]; skip.ID != 102 || skip.Reason != legacy.ReasonCutClash {
		t.Fatalf("пропуск назван не совпадением после резки: %+v", skip)
	}
}

// TestSecondPortChangesNothing: повторный запуск не плодит тем и не портит уже перенесённое —
// каждая тема называется пропущенной с причиной «уже в реестре», а файл остаётся байт в байт.
func TestSecondPortChangesNothing(t *testing.T) {
	t.Parallel()

	st := openStore(t)
	registry := read(t, `{"мост": 101, "шестой-этап": 202}`)

	topics := load(t, st)

	registry.Port(topics)
	save(t, st, topics)

	kept := registryBytes(t, st)
	report := registry.Port(load(t, st))

	if report.Read != 2 || report.Placed != 0 || len(report.Skipped) != 2 {
		t.Fatalf("второй перенос не назвал себя повторным: %+v", report)
	}

	for _, skip := range report.Skipped {
		if skip.Reason != legacy.ReasonInPlace {
			t.Fatalf("второй перенос назвал причину «%s» вместо «уже в реестре»", skip.Reason)
		}
	}

	topics = load(t, st)

	save(t, st, topics)

	if len(topics.ByName) != 2 || len(topics.ByID) != 2 {
		t.Fatalf("второй перенос размножил темы: %+v", topics)
	}

	if string(kept) != string(registryBytes(t, st)) {
		t.Fatal("второй перенос переписал реестр, хотя менять было нечего")
	}
}

// TestSkipIsNamed: занятое в новом реестре не затирается ни по имени, ни по id, и каждый пропуск
// называется своей причиной — новое состояние старше старого реестра.
func TestSkipIsNamed(t *testing.T) {
	t.Parallel()

	st := openStore(t)

	topics := load(t, st)
	topics.Put(store.Topic{ID: 900, Name: "занято", InMode: true, PinnedID: 55, IconUnknown: false})
	topics.Put(store.Topic{ID: 700, Name: "новое", InMode: false, PinnedID: 0, IconUnknown: false})

	report := read(t, `{"занято": 901, "другое": 700, "свободное": 800}`).Port(topics)

	if report.Read != 3 || report.Placed != 1 || len(report.Skipped) != 2 {
		t.Fatalf("пропуски не названы: %+v", report)
	}

	assertSkipped(t, report, map[string]legacy.Reason{
		"занято": legacy.ReasonNameTaken,
		"другое": legacy.ReasonIDTaken,
	})

	held, ok := topics.ByNameLookup("занято")
	if !ok || held.ID != 900 || !held.InMode || held.PinnedID != 55 {
		t.Fatalf("перенос затёр живую запись новой темой: %+v", held)
	}

	if _, ok = topics.ByNameLookup("свободное"); !ok {
		t.Fatal("свободная тема не легла в реестр")
	}
}

// TestBrokenStateRefused: из негодного состояния не переносится ничего, и отказ называет причину.
// Разбор идёт до записи целиком, поэтому половины перенесённого не бывает по устройству, а не по
// надежде: тест держит каждую форму негодности, которой старый мост не пишет.
func TestBrokenStateRefused(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"не JSON":              `{"мост":`,
		"не словарь":           `["мост"]`,
		"null":                 `null`,
		"пустой файл":          ``,
		"id строкой":           `{"мост": "101"}`,
		"id не целым числом":   `{"мост": 10.5}`,
		"id нулевой":           `{"мост": 0}`,
		"id отрицательный":     `{"мост": -101}`,
		"тема без имени":       `{"": 101}`,
		"id под двумя именами": `{"мост": 101, "шестой-этап": 101}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), legacy.TopicsFile)

			err := os.WriteFile(path, []byte(body), 0o600)
			if err != nil {
				t.Fatalf("состояние не записано: %v", err)
			}

			registry, err := legacy.ReadTopics(path)
			if !errors.Is(err, legacy.ErrBadState) {
				t.Fatalf("негодное состояние принято: %+v, %v", registry, err)
			}
		})
	}
}

// TestMissingStateRefused: файла нет — это отказ, а не пустой реестр. Пустой реестр означал бы
// «переносить нечего», и владелец, ошибшийся каталогом, узнал бы об этом только по пустым темам.
func TestMissingStateRefused(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), legacy.TopicsFile)

	_, err := legacy.ReadTopics(path)
	if !errors.Is(err, legacy.ErrBadState) {
		t.Fatalf("отсутствие реестра не названо отказом: %v", err)
	}
}

// assertTopic сверяет запись реестра по имени и обратным поиском: обе стороны словаря обязаны
// помнить перенесённую тему.
func assertTopic(t *testing.T, topics *store.Topics, name string, id int64) {
	t.Helper()

	topic, ok := topics.ByNameLookup(name)
	if !ok || topic.ID != id {
		t.Fatalf("тема «%s» не легла в реестр службы: %+v", name, topic)
	}

	if topic.InMode || topic.PinnedID != 0 {
		t.Fatalf("у перенесённой темы «%s» выдуман значок или закреплённая строка: %+v", name, topic)
	}

	// Что стоит у темы на самом деле, перенос не знает: «значок снят» было бы такой же выдумкой,
	// как «в режиме», и первое снятие режима пропустилось бы.
	if !topic.IconUnknown {
		t.Fatalf("у перенесённой темы «%s» значок назван известным: %+v", name, topic)
	}

	back, ok := topics.ByIDLookup(id)
	if !ok || back.Name != name {
		t.Fatalf("обратный поиск id %d не нашёл «%s»: %+v", id, name, back)
	}
}

// assertSkipped сверяет причины пропусков: каждая пропущенная тема названа и объяснена словами.
func assertSkipped(t *testing.T, report legacy.Report, want map[string]legacy.Reason) {
	t.Helper()

	for _, skip := range report.Skipped {
		reason, ok := want[skip.Name]
		if !ok {
			t.Fatalf("пропущена тема, которой не ждали: %+v", skip)
		}

		if skip.Reason != reason {
			t.Fatalf("тема «%s» пропущена по причине «%s», а ждали «%s»", skip.Name, skip.Reason, reason)
		}

		if skip.Reason.Text() == "" {
			t.Fatalf("пропуск «%s» не объяснён словами", skip.Name)
		}
	}
}

// read читает реестр старого моста из тела файла.
func read(t *testing.T, body string) legacy.Registry {
	t.Helper()

	path := filepath.Join(t.TempDir(), legacy.TopicsFile)

	err := os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatalf("состояние старого моста не записано: %v", err)
	}

	registry, err := legacy.ReadTopics(path)
	if err != nil {
		t.Fatalf("состояние старого моста не прочитано: %v", err)
	}

	return registry
}

// openStore открывает каталог состояния службы.
func openStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("состояние службы не открыто: %v", err)
	}

	return st
}

// load читает реестр тем службы; файла нет — пустой реестр.
func load(t *testing.T, st *store.Store) *store.Topics {
	t.Helper()

	topics, err := store.LoadTopics(st)
	if err != nil {
		t.Fatalf("реестр тем службы не прочитан: %v", err)
	}

	return topics
}

// save пишет реестр тем службы.
func save(t *testing.T, st *store.Store, topics *store.Topics) {
	t.Helper()

	err := topics.Save(st)
	if err != nil {
		t.Fatalf("реестр тем службы не записан: %v", err)
	}
}

// registryBytes отдаёт файл реестра как он лежит на диске.
func registryBytes(t *testing.T, st *store.Store) []byte {
	t.Helper()

	body, err := os.ReadFile(st.Path(legacy.TopicsFile))
	if err != nil {
		t.Fatalf("файл реестра не прочитан: %v", err)
	}

	return body
}
