//nolint:tagliatelle // имена полей состояния заданы контрактом моста и пишутся как в нём
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// inboxDir — каталог ящика: слова владельца, ещё не принятые агентом.
const inboxDir = "inbox"

// Record — запись ящика: слово владельца, которое агент ещё не подтвердил. Путь у слова владельца
// один — доставка соединению сессии, — поэтому вида записи у ящика нет. Запись несёт автора,
// провенанс и id сообщения в теме, чтобы отметка доставки нашла своё сообщение и после перезапуска
// службы.
type Record struct {
	Seq        uint64                `json:"seq"`
	Session    string                `json:"session"`
	ID         string                `json:"id"`
	Text       string                `json:"text"`
	Author     protocol.Author       `json:"author"`
	Provenance string                `json:"provenance"`
	MessageID  int                   `json:"message_id"`
	TopicID    int64                 `json:"topic_id"`
	TopicName  string                `json:"topic_name"`
	At         time.Time             `json:"at"`
	Delivery   protocol.DeliveryKind `json:"delivery_kind"`
}

// Validate проверяет инварианты записи. Провенанс автора — поле записи: его отсутствие ошибка,
// а не пустая строка.
func (r *Record) Validate() error {
	if r.Session == "" {
		return fmt.Errorf("%w: запись ящика без сессии", ErrBadRecord)
	}

	if r.ID == "" {
		return fmt.Errorf("%w: запись ящика без id", ErrBadRecord)
	}

	if r.Author.ID == 0 || r.Author.Name == "" || r.Author.Kind == "" {
		return fmt.Errorf("%w: запись ящика без автора", ErrBadRecord)
	}

	if r.Provenance == "" {
		return fmt.Errorf("%w: запись ящика без провенанса", ErrBadRecord)
	}

	return nil
}

// Save пишет запись ящика атомарной заменой.
func (r *Record) Save(st *Store) error {
	err := r.Validate()
	if err != nil {
		panic(err.Error())
	}

	return st.WriteJSON(r, inboxDir, r.Session, recordFile(r.Seq))
}

// Drop убирает запись ящика: приём подтверждён сессией.
func (r *Record) Drop(st *Store) error {
	err := os.Remove(st.Path(inboxDir, r.Session, recordFile(r.Seq)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errRemove(err)
	}

	return nil
}

// LoadInbox читает ящик целиком, в порядке монотонной последовательности службы, а не по именам
// файлов: у номера обновления меняется разрядность, и сортировка строк отдавала бы слова
// владельца не в том порядке, в котором он их писал.
func LoadInbox(st *Store) ([]*Record, error) {
	dirs, err := os.ReadDir(st.Path(inboxDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, errReadDir(err)
	}

	var records []*Record

	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}

		part, err := loadSessionInbox(st, dir.Name())
		if err != nil {
			return nil, err
		}

		records = append(records, part...)
	}

	sort.Slice(records, func(i, j int) bool { return records[i].Seq < records[j].Seq })

	return records, nil
}

// NewID готовит id записи: монотонная последовательность плюс случайный хвост, чтобы id не
// повторился после опустевшего ящика.
func NewID(prefix string, seq uint64) string {
	tail := make([]byte, 2)

	_, err := rand.Read(tail)
	if err != nil {
		panic("случайный хвост id записи не получен: " + err.Error())
	}

	return fmt.Sprintf("%s-%06d-%s", prefix, seq, hex.EncodeToString(tail))
}

// loadSessionInbox читает ящик одной сессии.
func loadSessionInbox(st *Store, session string) ([]*Record, error) {
	entries, err := os.ReadDir(st.Path(inboxDir, session))
	if err != nil {
		return nil, errReadDir(err)
	}

	records := make([]*Record, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		rec := &Record{} //nolint:exhaustruct // значения приходят из файла записи

		err = st.ReadJSON(rec, inboxDir, session, entry.Name())
		if err != nil {
			return nil, err
		}

		if rec.Session == "" {
			rec.Session = session
		}

		err = rec.Validate()
		if err != nil {
			return nil, err
		}

		records = append(records, rec)
	}

	return records, nil
}

// recordFile — имя файла записи: монотонный номер с ведущими нулями.
func recordFile(seq uint64) string {
	return fmt.Sprintf("%012d.json", seq)
}
