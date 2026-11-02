package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errLocked — отказ переименования поверх занятого файла: так Windows отвечает (`Access is denied`),
// когда на месте лежит бинарник идущей службы.
var errLocked = errors.New("доступ запрещён")

// errStuck — файл не встаёт на место ни при какой попытке.
var errStuck = errors.New("на место не встаёт")

// TestPlaceFileMovesLockedFileAside: поверх занятого файла переименование не идёт, поэтому прежний
// уводится в сторону, а новый встаёт на место. Без этого установка не меняла бы бинарник идущей
// службы вовсе.
func TestPlaceFileMovesLockedFileAside(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "telegram-bridge")
	temporary := filepath.Join(dir, "telegram-bridge.new")

	err := os.WriteFile(target, []byte("прежняя сборка"), 0o755)
	if err != nil {
		t.Fatalf("прежний бинарник не записан: %v", err)
	}

	err = os.WriteFile(temporary, []byte("новая сборка"), 0o755)
	if err != nil {
		t.Fatalf("новый бинарник не записан: %v", err)
	}

	refused := false

	locked := func(from, to string) error {
		if to == target && !refused {
			refused = true

			return errLocked // цель занята идущим процессом
		}

		return os.Rename(from, to)
	}

	err = placeFile(temporary, target, locked)
	if err != nil {
		t.Fatalf("новый файл не встал на место: %v", err)
	}

	placed, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("бинарник не прочитан: %v", err)
	}

	if string(placed) != "новая сборка" {
		t.Fatalf("на месте прежний бинарник: %q", placed)
	}

	_, err = os.Stat(target + asideSuffix)
	if !os.IsNotExist(err) {
		t.Fatalf("уведённый в сторону файл остался: %v", err)
	}
}

// TestPlaceFileRestoresLockedFile: новый файл не встал и после увода прежнего — прежний
// возвращается: машина не остаётся без бинарника службы.
func TestPlaceFileRestoresLockedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "telegram-bridge")
	temporary := filepath.Join(dir, "telegram-bridge.new")

	err := os.WriteFile(target, []byte("прежняя сборка"), 0o755)
	if err != nil {
		t.Fatalf("прежний бинарник не записан: %v", err)
	}

	err = os.WriteFile(temporary, []byte("новая сборка"), 0o755)
	if err != nil {
		t.Fatalf("новый бинарник не записан: %v", err)
	}

	broken := func(from, to string) error {
		if from == temporary {
			return errStuck // новый файл не встаёт на место ни при какой попытке
		}

		return os.Rename(from, to)
	}

	err = placeFile(temporary, target, broken)
	if !errors.Is(err, errStuck) {
		t.Fatalf("отказ подстановки не назван: %v", err)
	}

	placed, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("бинарник пропал: %v", err)
	}

	if string(placed) != "прежняя сборка" {
		t.Fatalf("на месте не прежний бинарник: %q", placed)
	}
}

// TestSweepAsideKeepsOnlyCopy: основного файла нет — уведённый в сторону остаётся единственной
// копией, и уборка его не трогает.
func TestSweepAsideKeepsOnlyCopy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "telegram-bridge")
	aside := target + asideSuffix

	err := os.WriteFile(aside, []byte("единственная копия"), 0o755)
	if err != nil {
		t.Fatalf("уведённый файл не записан: %v", err)
	}

	sweepAside(target)

	_, err = os.Stat(aside)
	if err != nil {
		t.Fatalf("уборка снесла единственную копию бинарника: %v", err)
	}

	err = os.WriteFile(target, []byte("основной файл"), 0o755)
	if err != nil {
		t.Fatalf("основной файл не записан: %v", err)
	}

	sweepAside(target)

	_, err = os.Stat(aside)
	if !os.IsNotExist(err) {
		t.Fatalf("уборка не сняла уведённый файл при живом основном: %v", err)
	}
}

// TestPlaceFileNamesLostFile: не удалась и подстановка нового файла, и возврат прежнего — отказ
// называет потерю бинарника службы, а не только неудачу подстановки.
func TestPlaceFileNamesLostFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "telegram-bridge")
	temporary := filepath.Join(dir, "telegram-bridge.new")

	err := os.WriteFile(target, []byte("прежняя сборка"), 0o755)
	if err != nil {
		t.Fatalf("прежний бинарник не записан: %v", err)
	}

	err = os.WriteFile(temporary, []byte("новая сборка"), 0o755)
	if err != nil {
		t.Fatalf("новый бинарник не записан: %v", err)
	}

	broken := func(from, to string) error {
		if to == target {
			return errStuck // на место ничего не встаёт: ни новый файл, ни возвращённый прежний
		}

		return os.Rename(from, to)
	}

	err = placeFile(temporary, target, broken)
	if !errors.Is(err, errStuck) {
		t.Fatalf("отказ не назвал причину: %v", err)
	}

	if !strings.Contains(err.Error(), "файла службы на диске нет") {
		t.Fatalf("отказ не назвал потерянный бинарник: %v", err)
	}
}
