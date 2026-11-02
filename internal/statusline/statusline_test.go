package statusline_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/statusline"
)

// session — id сессии в стенде: вклад моста живёт только в области сессии.
const session = "ses-01"

// contribution — файл вклада, разобранный тестом по контракту статусной строки.
type contribution struct {
	V        int     `json:"v"`
	Provider string  `json:"provider"`
	At       float64 `json:"at"`
	TTL      int     `json:"ttl"`
	Items    []struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		Row   int    `json:"row"`
		Order int    `json:"order"`
		Parts []struct {
			Text  string `json:"text"`
			Color string `json:"color"`
		} `json:"parts"`
		URI   string `json:"uri"`
		Error string `json:"error"`
	} `json:"items"`
	Bash struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	} `json:"bash"`
}

// TestModeOnPutsTopicIntoCubeTitle: включённый режим даёт ровно один сегмент заголовка кубика
// сессии с именем темы, её ссылкой и ролью палитры, плюс результат зонда со словом признака.
func TestModeOnPutsTopicIntoCubeTitle(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writer := statusline.New(root)

	err := writer.Write(session, statusline.State{
		Mode: true, TopicName: "06-служба", TopicURI: "https://t.me/c/500/42", Fault: "",
	})
	if err != nil {
		t.Fatalf("вклад не записан: %v", err)
	}

	got := read(t, writer.SegmentPath(session))

	checkEnvelope(t, got)
	checkTitle(t, got)

	word, err := os.ReadFile(writer.ProbePath(session))
	if err != nil {
		t.Fatalf("результат зонда не записан: %v", err)
	}

	if strings.TrimRight(string(word), "\r\n") != statusline.ProbeWord {
		t.Fatalf("в результате зонда %q, а правило сравнивает точно со %q", word, statusline.ProbeWord)
	}
}

// TestModeOffLeavesLivingProviderWithoutItems: снятый режим оставляет файл вклада с пустым
// списком — «поставщик жив, показывать нечего», — и убирает результат зонда: нет файла, нет и
// цвета заголовка.
func TestModeOffLeavesLivingProviderWithoutItems(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writer := statusline.New(root)

	err := writer.Write(session, statusline.State{
		Mode: true, TopicName: "тема", TopicURI: "https://t.me/c/500/42", Fault: "",
	})
	if err != nil {
		t.Fatalf("вклад не записан: %v", err)
	}

	err = writer.Write(session, statusline.State{Mode: false, TopicName: "тема", TopicURI: "", Fault: ""})
	if err != nil {
		t.Fatalf("снятие режима не записано: %v", err)
	}

	got := read(t, writer.SegmentPath(session))
	if len(got.Items) != 0 {
		t.Fatalf("со снятым режимом сегментов %d, а должен быть пустой список", len(got.Items))
	}

	if got.Bash.Name != "" {
		t.Fatalf("заплатка bash-читателя держит имя темы снятого режима: %q", got.Bash.Name)
	}

	_, err = os.Stat(writer.ProbePath(session))
	if !os.IsNotExist(err) {
		t.Fatalf("результат зонда остался при снятом режиме: %v", err)
	}
}

// TestFaultRidesTheSameContribution: отказ самого моста уезжает полем `error` того же вклада, а
// имя темы при включённом режиме остаётся рядом — bash-читатель `error` не знает, и без кусков
// режим пропал бы у него с экрана вовсе.
func TestFaultRidesTheSameContribution(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writer := statusline.New(root)

	err := writer.Write(session, statusline.State{
		Mode: true, TopicName: "тема", TopicURI: "", Fault: "network: сеть не отвечает",
	})
	if err != nil {
		t.Fatalf("вклад не записан: %v", err)
	}

	got := read(t, writer.SegmentPath(session))
	if len(got.Items) != 1 || got.Items[0].Error != "network: сеть не отвечает" {
		t.Fatalf("отказ моста не попал в вклад: %+v", got.Items)
	}

	if len(got.Items[0].Parts) != 1 {
		t.Fatalf("имя темы пропало вместе с отказом: %+v", got.Items[0])
	}
}

// TestReplaceNeverLeavesTargetMissing: замена идёт временным файлом в том же каталоге и
// переименованием, без предварительного удаления цели, и временных файлов после себя не
// оставляет: между удалением и переименованием читатель увидел бы отсутствие вклада.
func TestReplaceNeverLeavesTargetMissing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writer := statusline.New(root)
	state := statusline.State{Mode: true, TopicName: "первая", TopicURI: "", Fault: ""}

	for _, name := range []string{"первая", "вторая", "третья"} {
		state.TopicName = name

		err := writer.Write(session, state)
		if err != nil {
			t.Fatalf("вклад не записан: %v", err)
		}

		if read(t, writer.SegmentPath(session)).Items[0].Parts[0].Text != name {
			t.Fatalf("вклад не заменился целиком на %q", name)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(writer.SegmentPath(session)))
	if err != nil {
		t.Fatalf("каталог вкладов не прочитан: %v", err)
	}

	for _, entry := range entries {
		if entry.Name() != statusline.Provider+".json" {
			t.Fatalf("в каталоге вкладов остался лишний файл %q", entry.Name())
		}
	}
}

// TestNoRootNoWriting: корня нет — мост не пишет ничего, и это не ошибка: читателя он не ищет.
// Такой корень бывает, когда в окружении нет домашнего каталога.
func TestNoRootNoWriting(t *testing.T) {
	t.Parallel()

	writer := statusline.New("")

	err := writer.Write(session, statusline.State{Mode: true, TopicName: "тема", TopicURI: "", Fault: ""})
	if err != nil {
		t.Fatalf("писатель без корня назвал отказ: %v", err)
	}

	if writer.Root() != "" {
		t.Fatalf("писатель без корня придумал себе корень %q", writer.Root())
	}
}

// TestSessionlessContributionPanics: область вклада моста — сессия, и `shared` у него не бывает:
// вклад без id сессии — нарушенный инвариант, а не файл в общей области.
func TestSessionlessContributionPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("вклад без id сессии записан молча")
		}
	}()

	_ = statusline.New(t.TempDir()).Write("", statusline.State{
		Mode: true, TopicName: "тема", TopicURI: "", Fault: "",
	})
}

// checkEnvelope сверяет конверт вклада с контрактом строки: версия, поставщик, свежесть.
func checkEnvelope(t *testing.T, got contribution) {
	t.Helper()

	if got.V != statusline.Version || got.Provider != statusline.Provider {
		t.Fatalf("конверт не по контракту: v=%d provider=%q", got.V, got.Provider)
	}

	if got.TTL != statusline.TTL {
		t.Fatalf("срок годности %d, а назначен %d", got.TTL, statusline.TTL)
	}

	if got.At+float64(got.TTL) < float64(time.Now().Unix()) {
		t.Fatalf("вклад записан просроченным: at=%f ttl=%d", got.At, got.TTL)
	}
}

// checkTitle сверяет сегмент заголовка кубика сессии: имя темы, её цвет, ссылка и заплатка
// bash-читателя.
func checkTitle(t *testing.T, got contribution) {
	t.Helper()

	if len(got.Items) != 1 {
		t.Fatalf("сегментов %d, а режим даёт ровно один", len(got.Items))
	}

	item := got.Items[0]
	if item.ID != statusline.SegmentID || item.Kind != statusline.SegmentKind {
		t.Fatalf("сегмент назван не по контракту: id=%q kind=%q", item.ID, item.Kind)
	}

	// `row: 0` — заголовок кубика сессии. Отсутствие поля читатель понял бы как `1`, то есть
	// как строку тела, и режим уехал бы из заголовка.
	if item.Row != 0 || item.Order != 0 {
		t.Fatalf("строка и порядок сегмента %d/%d, а заголовку нужны 0/0", item.Row, item.Order)
	}

	checkName(t, got)
}

// checkName сверяет имя темы, его цвет, ссылку на тему и заплатку bash-читателя.
func checkName(t *testing.T, got contribution) {
	t.Helper()

	item := got.Items[0]

	if len(item.Parts) != 1 {
		t.Fatalf("кусков в сегменте %d, а имя темы — один: %+v", len(item.Parts), item.Parts)
	}

	if item.Parts[0].Text != "06-служба" || item.Parts[0].Color != statusline.NameColor {
		t.Fatalf("имя темы или его цвет не те: %+v", item.Parts[0])
	}

	if item.URI != "https://t.me/c/500/42" {
		t.Fatalf("ссылка на тему не та: %q", item.URI)
	}

	if got.Bash.Name != "06-служба" || got.Bash.URI != "https://t.me/c/500/42" {
		t.Fatalf("заплатка bash-читателя не заполнена: %+v", got.Bash)
	}
}

// read читает файл вклада и разбирает его.
func read(t *testing.T, path string) contribution {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("вклад не прочитан: %v", err)
	}

	var got contribution

	err = json.Unmarshal(body, &got)
	if err != nil {
		t.Fatalf("вклад не разобран (%s): %v", body, err)
	}

	return got
}
