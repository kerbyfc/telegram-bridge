package install

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// asideSuffix — хвост имени файла, уведённого в сторону: так уходит заблокированный бинарник идущей
// службы, на место которого встаёт новый.
const asideSuffix = ".old"

// ExecRunner — настоящий запуск команд ОС.
func ExecRunner(name string, args ...string) (string, error) {
	// Контекста у шага нет: команды автозапуска короткие, и отменять их установка не умеет.
	output, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec,noctx // команды ОС, аргументы свои
	if err != nil {
		return string(output), fmt.Errorf(
			"%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)),
		)
	}

	return string(output), nil
}

// placeBinary кладёт бинарник службы в каталог запуска. Копия, а не ссылка на дерево сборки:
// служба обязана подниматься и тогда, когда worktree, в котором её собрали, уже убран.
//
// Источник пуст — берётся свой исполняемый файл: установку зовут тем же бинарём, который и станет
// службой.
func placeBinary(source, target string) (Change, error) {
	sweepAside(target)

	if source == "" {
		own, err := os.Executable()
		if err != nil {
			return Change{Done: false, Note: ""}, fmt.Errorf(
				"%w: свой исполняемый файл неизвестен: %w", ErrInstall, err,
			)
		}

		source = own
	}

	body, err := os.ReadFile(source)
	if err != nil {
		return Change{Done: false, Note: ""}, fmt.Errorf("%w: %s: %w", ErrInstall, source, err)
	}

	same, err := sameContent(target, body)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	if same {
		return Change{Done: false, Note: "тот же, " + target}, nil
	}

	err = writeFile(target, body, binaryPerm)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	return Change{Done: true, Note: "поставлен, " + target}, nil
}

// sameContent — файл есть, и его содержимое ровно `content`. На этом стоит идемпотентность:
// совпало — шаг не переделывается, и живая служба не рвётся на пустом месте.
func sameContent(path string, content []byte) (bool, error) {
	current, err := os.ReadFile(path)

	switch {
	case err == nil:
		return bytes.Equal(current, content), nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("%w: %s: %w", ErrInstall, path, err)
	}
}

// writeFile пишет файл через временный и переименование: заменяемый файл не бывает полупустым, а
// идущий бинарник на месте перезаписать нельзя вовсе — заменить его переименованием можно.
func writeFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)

	err := os.MkdirAll(dir, dirPerm)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInstall, dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInstall, dir, err)
	}

	name := tmp.Name()

	err = fillFile(tmp, content, mode)
	if err != nil {
		_, _ = dropFile(name)

		return err
	}

	err = placeFile(name, path, os.Rename)
	if err != nil {
		_, _ = dropFile(name)

		return err
	}

	return nil
}

// placeFile ставит готовый файл на место. Простое переименование Windows отвергает, когда на месте
// идущий бинарник: файл образа заблокирован, перезаписать его нельзя, а переименовать можно. Тогда
// прежний файл уводится в сторону, новый встаёт на место, а уведённый удаляется, когда его отпустит
// прежний процесс, — на этом запуске или на следующем. Новый файл не встал — прежний возвращается,
// иначе машина осталась бы без бинарника.
func placeFile(temporary, path string, rename func(from, to string) error) error {
	err := rename(temporary, path)
	if err == nil {
		return nil
	}

	aside, asideErr := moveAside(path, rename)
	if asideErr != nil {
		return fmt.Errorf("%w: %s: %w", ErrInstall, path, err)
	}

	err = rename(temporary, path)
	if err != nil {
		restoreErr := rename(aside, path)
		if restoreErr != nil {
			return fmt.Errorf(
				"%w: %s: новый файл не встал (%w), и прежний не вернулся (%w): файла службы на диске нет",
				ErrInstall, path, err, restoreErr,
			)
		}

		return fmt.Errorf("%w: %s: %w", ErrInstall, path, err)
	}

	_, _ = dropFile(aside)

	return nil
}

// moveAside уводит лежащий файл в сторону и отдаёт его новый адрес.
func moveAside(path string, rename func(from, to string) error) (string, error) {
	aside := path + asideSuffix

	_, err := dropFile(aside)
	if err != nil {
		// Прежний уведённый файл ещё держит процесс — имя берётся другое, а уборкой занимается
		// следующий запуск.
		aside = path + asideSuffix + "." + strconv.FormatInt(time.Now().UnixNano(), 10)
	}

	err = rename(path, aside)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrInstall, path, err)
	}

	return aside, nil
}

// sweepAside убирает файлы, уведённые в сторону прошлыми установками: процесс, который их держал,
// давно вышел. Основного файла нет — уборки нет: уведённый остался единственной копией, и снести её
// значит оставить машину без бинарника службы. Отказ уборки — не отказ установки: файл ещё держат,
// и он уйдёт позже.
func sweepAside(path string) {
	_, err := os.Stat(path)
	if err != nil {
		return
	}

	leftovers, err := filepath.Glob(path + asideSuffix + "*")
	if err != nil {
		return
	}

	for _, leftover := range leftovers {
		_, _ = dropFile(leftover)
	}
}

// fillFile пишет содержимое, ставит права и закрывает файл.
func fillFile(file *os.File, content []byte, mode os.FileMode) error {
	_, err := file.Write(content)
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("%w: %s: %w", ErrInstall, file.Name(), err)
	}

	err = file.Chmod(mode)
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("%w: %s: %w", ErrInstall, file.Name(), err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInstall, file.Name(), err)
	}

	return nil
}

// dropFile удаляет файл; его отсутствие — не ошибка, а «удалять нечего».
func dropFile(path string) (bool, error) {
	if path == "" {
		return false, nil
	}

	err := os.Remove(path)

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("%w: %s: %w", ErrInstall, path, err)
	}
}
