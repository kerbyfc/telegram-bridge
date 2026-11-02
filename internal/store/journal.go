//nolint:tagliatelle // имена полей состояния заданы контрактом моста и пишутся как в нём
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrJournal — журнал службы не ведётся.
var ErrJournal = errors.New("журнал службы отказал")

// Journal — журнал службы: отказы, отброшенные сообщения, расхождения версий. Дописывание в
// конец одним писателем, а не чтение-правка-запись: журнал, который правят два процесса врозь,
// теряет записи и оставляет недописанную строку, после которой разбор падает.
//
// Файл меняет поколение по размеру: нынешний уезжает в `.1`, прежние сдвигаются дальше, а старше
// `keep` штук не хранится.
type Journal struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	maxBytes int64
	keep     int
	size     int64
}

// Entry — строка журнала.
type Entry struct {
	At      time.Time `json:"at"`
	Event   string    `json:"event"`
	Session string    `json:"session,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Code    string    `json:"code,omitempty"`
	Text    string    `json:"text,omitempty"`
	TopicID int64     `json:"topic_id,omitempty"`
}

// Write дописывает строку журнала целиком, одной записью. Обрывок, который оставил отказ записи,
// закрывает переводом строки следующее открытие журнала (`openLog`), и с новой строкой он не
// склеивается.
func (j *Journal) Write(entry Entry) error {
	if entry.At.IsZero() {
		entry.At = time.Now()
	}

	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("%w: строка журнала не собрана: %w", ErrJournal, err)
	}

	body = append(body, '\n')

	j.mu.Lock()
	defer j.mu.Unlock()

	err = j.turnOver(int64(len(body)))
	if err != nil {
		return err
	}

	written, err := j.file.Write(body)
	j.size += int64(written)

	if err != nil {
		return fmt.Errorf("%w: строка журнала не записана: %w", ErrJournal, err)
	}

	return nil
}

// Path отдаёт путь нынешнего поколения журнала.
func (j *Journal) Path() string {
	return j.path
}

// Close закрывает журнал.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	err := j.file.Close()
	if err != nil {
		return fmt.Errorf("%w: журнал не закрыт: %w", ErrJournal, err)
	}

	return nil
}

// turnOver меняет поколение, если строка не укладывается в предел. Пустой файл не меняется
// никогда: строка длиннее предела обязана лечь целиком, а не пропасть.
func (j *Journal) turnOver(adding int64) error {
	if j.size == 0 || j.size+adding <= j.maxBytes {
		return nil
	}

	err := j.file.Close()
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrJournal, j.path, err)
	}

	// Самое старое поколение уходит целиком: держать больше `keep` штук незачем.
	err = dropGeneration(j.generation(j.keep))
	if err != nil {
		return err
	}

	for index := j.keep - 1; index >= 0; index-- {
		err = shiftGeneration(j.generation(index), j.generation(index+1))
		if err != nil {
			return err
		}
	}

	file, size, err := openLog(j.path)
	if err != nil {
		return err
	}

	j.file = file
	j.size = size

	return nil
}

// generation — имя файла поколения: 0 — нынешний, дальше `.1`, `.2` и так до `keep`.
func (j *Journal) generation(index int) string {
	if index == 0 {
		return j.path
	}

	return j.path + "." + strconv.Itoa(index)
}

// OpenJournal открывает журнал на дописывание, создавая его при надобности. Предел поколения и
// число хранимых поколений приходят из настроек; негодные значения — отказ, а не тихая подмена
// своими: журнал без ротации растёт без предела, и молча решать это за владельца нельзя.
func OpenJournal(path string, maxBytes int64, keep int) (*Journal, error) {
	if maxBytes <= 0 || keep < 1 {
		return nil, fmt.Errorf(
			"%w: ротации нужен предел больше нуля и хотя бы одно поколение, названо %d байт и %d поколений",
			ErrJournal, maxBytes, keep,
		)
	}

	file, size, err := openLog(path)
	if err != nil {
		return nil, err
	}

	return &Journal{
		mu:       sync.Mutex{},
		file:     file,
		path:     path,
		maxBytes: maxBytes,
		keep:     keep,
		size:     size,
	}, nil
}

// TailJournal читает последние `lines` строк журнала: ими самопроверка называет причину отказа,
// когда службы уже нет и спросить её нечем. Журнала нет — строк нет, и это не ошибка: служба
// могла ни разу не подниматься.
func TailJournal(path string, lines int) ([]Entry, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrJournal, path, err)
	}

	defer func() { _ = file.Close() }()

	kept := make([]Entry, 0, lines)
	reader := bufio.NewScanner(file)
	reader.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for reader.Scan() {
		var entry Entry

		// Недописанную строку разбор пропускает: она значит обрыв записи, а не конец журнала,
		// и валить на ней чтение хвоста незачем — остальные строки целы.
		if json.Unmarshal(reader.Bytes(), &entry) != nil {
			continue
		}

		if len(kept) == lines {
			kept = kept[1:]
		}

		kept = append(kept, entry)
	}

	err = reader.Err()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrJournal, path, err)
	}

	return kept, nil
}

// TailLines читает последние `lines` строк любого текстового файла. Этим самопроверка достаёт
// хвост вывода самого процесса службы: там лежит то, что минует журнал, — паника и отказы до его
// открытия, — и строки там не JSON, а текст как есть. Файла нет — строк нет, и это не ошибка.
func TailLines(path string, lines int) ([]string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrJournal, path, err)
	}

	defer func() { _ = file.Close() }()

	kept := make([]string, 0, lines)
	reader := bufio.NewScanner(file)
	reader.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for reader.Scan() {
		text := strings.TrimSpace(reader.Text())
		if text == "" {
			continue
		}

		if len(kept) == lines {
			kept = kept[1:]
		}

		kept = append(kept, text)
	}

	err = reader.Err()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrJournal, path, err)
	}

	return kept, nil
}

// openLog открывает файл журнала на дописывание и отдаёт его нынешний размер.
//
// Файл, кончившийся недописанной строкой, сначала получает перевод строки. Такой хвост оставляет
// обрыв записи — полный диск, отказ питания, — и служба на нём падает: отказ журнала у неё паника.
// Без перевода строки первая же новая строка поднявшейся службы склеилась бы с обрывком и не
// разобралась бы вместе с ним — пропала бы как раз запись о том, что было после отказа. Обрывок
// остаётся на месте своей строкой: след отказа не стирается.
func openLog(path string) (*os.File, int64, error) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, FilePerm)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %s: %w", ErrJournal, path, err)
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()

		return nil, 0, fmt.Errorf("%w: %s: %w", ErrJournal, path, err)
	}

	size := info.Size()
	if size == 0 {
		return file, 0, nil
	}

	last := make([]byte, 1)

	_, err = file.ReadAt(last, size-1)
	if err != nil {
		_ = file.Close()

		return nil, 0, fmt.Errorf("%w: хвост %s не прочитан: %w", ErrJournal, path, err)
	}

	if last[0] == '\n' {
		return file, size, nil
	}

	written, err := file.Write([]byte{'\n'})
	if err != nil {
		_ = file.Close()

		return nil, 0, fmt.Errorf("%w: недописанная строка %s не закрыта: %w", ErrJournal, path, err)
	}

	return file, size + int64(written), nil
}

// dropGeneration удаляет поколение журнала; его отсутствие — не ошибка.
func dropGeneration(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s: %w", ErrJournal, path, err)
	}

	return nil
}

// shiftGeneration сдвигает поколение журнала; его отсутствие — не ошибка.
func shiftGeneration(from, to string) error {
	err := os.Rename(from, to)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s -> %s: %w", ErrJournal, from, to, err)
	}

	return nil
}
