// Package statusline — вклад моста в статусную строку. Мост — поставщик
// `telegram-bridge`, область у него одна — сессия: имя темы, признак режима и ссылка на тему
// связаны с сессией один к одному, общемашинного вклада у моста нет.
//
// Файла два, и один другого не заменяет: вклад даёт текст заголовка кубика сессии, результат
// вырожденного зонда — состояние датчика для правила окраски заголовка. Писать нужно оба.
//
// Строка состояния внутренностей моста не читает и о его устройстве ничего не знает; мост, в свою
// очередь, не читает ни одного файла строки и её отсутствие ошибкой не считает.
package statusline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Provider — имя поставщика: оно же имя файла вклада и префикс id его кубиков.
const Provider = "telegram-bridge"

// Version — версия формата вклада; `v` выше известной читатель не разбирает вовсе.
const Version = 1

// TTL — срок годности вклада в секундах. Сутки, и это законно ровно потому, что у службы есть
// свой такт записи: пока режим включён, она переписывает оба файла раз в час, а контракт строки
// требует срок не короче интервала между своими записями. Без такта тот же срок врал бы — режим,
// включённый дольше суток, уехал бы с экрана сам.
//
// Запас между тактом и сроком нужен: пропущенный такт — перезапуск службы, занятый диск — не
// должен снимать режим с экрана. Бесконечного срока контракт строки не знает.
const TTL = 86400

// SegmentID — id сегмента в пространстве имён поставщика; полный id читатель собирает сам.
const SegmentID = "mode"

// SegmentKind — вид вклада: сегмент кубика сессии, а не свой кубик.
const SegmentKind = "segment"

// TitleRow — строка кубика сессии: заголовок. Режим владелец должен видеть первым.
const TitleRow = 0

// NameColor — роль палитры для имени темы: тот жёлтый, которым строка печатала имя раньше.
const NameColor = "yellow"

// ProbeName — имя файла результата зонда правила окраски заголовка. Без расширения: правило
// порогов называет путь `{root}/statusline/probes/{session}/away`, и имя файла здесь — имя
// признака режима, а не конверт писателя.
const ProbeName = "away"

// ProbeWord — результат зонда: ровно признак режима. Сравнение с перечнем правила точное —
// регистр и пробелы значимы, читатель снимает только перевод строки в конце.
const ProbeWord = "away"

// dirPerm и filePerm — права каталогов и файлов вклада. Каталог состояния строки — чужой, и
// права здесь те же, которыми его заводит сама строка: читатель и писатель ходят под одним
// пользователем.
const (
	dirPerm  os.FileMode = 0o755
	filePerm os.FileMode = 0o644
)

// ErrWrite — вклад не лёг на диск. Отказ записи для моста не отказ работы: строка — чужой
// читатель, и его отсутствие ошибкой не считается.
var ErrWrite = errors.New("вклад в статусную строку не записан")

// State — то, что мост говорит строке о сессии.
type State struct {
	// Mode — включён ли режим «не у компьютера». Он и есть признак непустого вклада.
	Mode bool
	// TopicName — имя темы, которое встаёт в заголовок кубика сессии.
	TopicName string
	// TopicURI — ссылка на саму тему; адресовать нечего — пусто, и заголовок сохраняет свой
	// прежний адрес, а не получает пустой.
	TopicURI string
	// Fault — отказ самого моста (`no-owner`, `version`, `network`, `tls-untrusted`). Режим
	// обязан быть виден владельцу и когда показывать нечего: это аварийный канал.
	Fault string
}

// part — кусок текста сегмента.
type part struct {
	Text  string `json:"text"`
	Color string `json:"color,omitempty"`
}

// item — один вклад поставщика. `row` и `order` пишутся всегда: у отсутствующего `row`
// читатель берёт значение по умолчанию `1`, то есть тело кубика, а заголовку нужен `0`.
type item struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Row   int    `json:"row"`
	Order int    `json:"order"`
	Parts []part `json:"parts,omitempty"`
	URI   string `json:"uri,omitempty"`
	Error string `json:"error,omitempty"`
}

// bashPatch — заплатка одного-единственного потребителя: нынешнего bash-читателя
// (`scripts/statusline.sh`), у которого нет и не заводится разборщик `parts`. Пока он жив, без
// этого поля режим на экране не появится, хотя генерик-вид читает ядро на Go. Поля два: имя темы
// и её адрес. Условие снятия названо контрактом строки: не станет bash-читателя — перестанет
// писаться и поле.
//
// Пределы заплатки: цвет имени bash-читатель берёт жёстким и `color` вклада игнорирует, а `uri`
// в однострочном виде не использует вовсе. Отказ моста в неё не попадает — своего поля для него
// у заплатки нет.
type bashPatch struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
}

// envelope — файл вклада целиком: конверт контракта плюс заплатка bash-читателя.
type envelope struct {
	V        int       `json:"v"`
	Provider string    `json:"provider"`
	At       float64   `json:"at"`
	TTL      int       `json:"ttl"`
	Items    []item    `json:"items"`
	Bash     bashPatch `json:"bash"`
}

// Writer — писатель вклада. Корень состояния строки мост получает из настроек службы и сам его не
// выводит: разойдись он с читателем, вклад просто не найдётся.
type Writer struct {
	root string
	now  func() float64
}

// Root отдаёт корень состояния строки; пусто — писать некуда.
func (w *Writer) Root() string {
	return w.root
}

// SegmentPath — файл вклада этой сессии.
func (w *Writer) SegmentPath(session string) string {
	return filepath.Join(w.root, "statusline", "segments", session, Provider+".json")
}

// ProbePath — файл результата зонда этой сессии.
func (w *Writer) ProbePath(session string) string {
	return filepath.Join(w.root, "statusline", "probes", session, ProbeName)
}

// Write кладёт вклад и результат зонда. Зовётся там, где мост меняет своё состояние режима, а не
// по запросу строки: тик поставщиков не запускает и не ждёт — он читает то, что уже лежит.
//
// Нет корня — не пишется ничего, и это не ошибка: читателя мост не ищет. Корня не бывает, когда
// в окружении нет домашнего каталога: ключ настроек и `~/.claude` разрешает config.
func (w *Writer) Write(session string, state State) error {
	if w.root == "" {
		return nil
	}

	if session == "" {
		panic("вклад моста без id сессии: область вклада — сессия, и `shared` у моста не бывает")
	}

	body, err := json.Marshal(envelope{
		V:        Version,
		Provider: Provider,
		At:       w.now(),
		TTL:      TTL,
		Items:    items(state),
		Bash:     patch(state),
	})
	if err != nil {
		return fmt.Errorf("%w: конверт не собран: %w", ErrWrite, err)
	}

	err = replace(w.SegmentPath(session), append(body, '\n'))
	if err != nil {
		return err
	}

	// Результат зонда живёт ровно пока режим включён: нет файла, другое слово или просроченный
	// файл — цвета нет вовсе, и мёртвый датчик не выглядит как живой, которому нечего сказать.
	if !state.Mode {
		return drop(w.ProbePath(session))
	}

	return replace(w.ProbePath(session), []byte(ProbeWord+"\n"))
}

// items собирает вклад. Режим включён — ровно один сегмент с именем темы; режим выключен —
// пустой массив, то есть «поставщик жив, показывать нечего».
//
// Отказ самого моста идёт полем `error` того же сегмента: читатель печатает его на месте кусков,
// и владелец видит причину там, где ждал имя темы. При включённом режиме куски остаются рядом с
// отказом — bash-читатель `error` не знает, и без кусков режим пропал бы у него с экрана вовсе.
func items(state State) []item {
	if !state.Mode && state.Fault == "" {
		return []item{}
	}

	one := item{
		ID:    SegmentID,
		Kind:  SegmentKind,
		Row:   TitleRow,
		Order: 0,
		Parts: nil,
		URI:   "",
		Error: state.Fault,
	}

	if state.Mode {
		one.Parts = []part{{Text: state.TopicName, Color: NameColor}}
		one.URI = state.TopicURI
	}

	return []item{one}
}

// patch собирает заплатку bash-читателя: имя темы и её адрес словами, без разбора кусков.
func patch(state State) bashPatch {
	if !state.Mode {
		return bashPatch{Name: "", URI: ""}
	}

	return bashPatch{Name: state.TopicName, URI: state.TopicURI}
}

// replace заменяет файл целиком и атомарно: временный файл с ведущей точкой в том же каталоге,
// затем переименование. Тот же каталог обязателен — `rename` атомарен только внутри одного тома.
// Предварительно удалять цель нельзя: между удалением и переименованием читатель увидел бы
// отсутствие вклада.
func replace(path string, body []byte) error {
	dir := filepath.Dir(path)

	err := os.MkdirAll(dir, dirPerm)
	if err != nil {
		return fmt.Errorf("%w: каталог %s: %w", ErrWrite, dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+Provider+"-*")
	if err != nil {
		return fmt.Errorf("%w: временный файл в %s: %w", ErrWrite, dir, err)
	}

	name := tmp.Name()

	err = fill(tmp, body)
	if err != nil {
		_ = os.Remove(name)

		return err
	}

	err = os.Rename(name, path)
	if err != nil {
		_ = os.Remove(name)

		return fmt.Errorf("%w: %s: %w", ErrWrite, path, err)
	}

	return nil
}

// fill пишет тело, ставит права и закрывает файл: полупустого вклада читателю не достаётся.
func fill(file *os.File, body []byte) error {
	_, err := file.Write(body)
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("%w: %s: %w", ErrWrite, file.Name(), err)
	}

	err = file.Chmod(filePerm)
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("%w: %s: %w", ErrWrite, file.Name(), err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrWrite, file.Name(), err)
	}

	return nil
}

// drop убирает результат зонда: его отсутствие и значит «режима нет».
func drop(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s: %w", ErrWrite, path, err)
	}

	return nil
}

// New готовит писателя на корне состояния строки. Пустой корень — писатель молчит.
func New(root string) *Writer {
	return &Writer{root: root, now: unixNow}
}

// NewAt готовит писателя со своими часами: ими тесты проверяют срок годности вклада.
func NewAt(root string, now func() float64) *Writer {
	return &Writer{root: root, now: now}
}

// unixNow — время подготовки вклада в unix-секундах с дробной частью, как его ждёт конверт.
func unixNow() float64 {
	return float64(time.Now().UnixNano()) / float64(time.Second)
}
