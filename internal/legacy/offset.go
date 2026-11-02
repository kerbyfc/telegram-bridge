package legacy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// OffsetFile — смещение опроса старого моста в каталоге его состояния: голое число, id первого
// обновления, которого старый мост ещё не разобрал (`update_id + 1` последнего разобранного).
const OffsetFile = "offset"

// OffsetOutcome — что перенос сделал со смещением опроса. Значений ровно три, по одному на случай.
type OffsetOutcome string

const (
	// OffsetNone — в старом состоянии смещения нет: переносить нечего, и это не отказ.
	OffsetNone OffsetOutcome = "none"
	// OffsetPlaced — смещение старого моста легло в состояние службы.
	OffsetPlaced OffsetOutcome = "placed"
	// OffsetKept — смещение службы не меньше старого и остаётся: назад смещение не идёт.
	OffsetKept OffsetOutcome = "kept"
)

// ReadOffset читает смещение опроса старого моста. Файла нет — смещения нет: это нуль, а не отказ,
// — старый мост, не разобравший ни одного обновления, файла не заводит. Негодно всё, чего старый
// мост записать не мог: не целое число и число не больше нуля, потому что он пишет id обновления
// плюс один. Пробелы по краям допускаются: их терпит и чтение самого старого моста.
func ReadOffset(path string) (int64, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrBadState, path, err)
	}

	text := strings.TrimSpace(string(body))

	offset, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: смещение опроса %q не целое число", ErrBadState, path, text)
	}

	if offset <= 0 {
		return 0, fmt.Errorf("%w: %s: смещение опроса %d не больше нуля, а старый мост пишет id обновления "+
			"плюс один", ErrBadState, path, offset)
	}

	return offset, nil
}

// PortOffset кладёт смещение опроса старого моста в состояние службы, и служба начинает с него
// первый опрос. Так Telegram подтверждает обновление, которое старый мост разобрал, но подтвердить
// не успел — подтверждает он его только следующим опросом, — и новой службе оно второй раз не
// приходит.
//
// Назад смещение не идёт: служба, ушедшая дальше старого моста, уже разобрала больше, и откат
// вернул бы ей разобранные слова владельца второй раз.
func PortOffset(old int64, updates *store.Updates) OffsetOutcome {
	switch {
	case old == 0:
		return OffsetNone
	case updates.Offset >= old:
		return OffsetKept
	default:
		updates.Offset = old

		return OffsetPlaced
	}
}
