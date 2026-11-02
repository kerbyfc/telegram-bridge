// Package store — состояние службы на диске: только то, что обязано переживать перезапуск.
// Каталоги 0700, файлы 0600, запись — атомарная замена через временный файл в том же каталоге.
// Файловых замков нет ни одного: писатель один — служба.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DirPerm — права каталогов состояния.
const DirPerm = 0o700

// FilePerm — права файлов состояния.
const FilePerm = 0o600

// ErrNotFound — файла состояния нет.
var ErrNotFound = errors.New("записи состояния нет")

// Store — корень состояния службы.
type Store struct {
	root string
}

// Root отдаёт корень состояния.
func (s *Store) Root() string {
	return s.root
}

// Path собирает путь внутри корня состояния.
func (s *Store) Path(parts ...string) string {
	return filepath.Join(append([]string{s.root}, parts...)...)
}

// WriteJSON пишет объект атомарной заменой.
func (s *Store) WriteJSON(value any, parts ...string) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("состояние не собрано: %w", err)
	}

	return WriteAtomic(s.Path(parts...), append(body, '\n'))
}

// ReadJSON читает объект; нет файла — ErrNotFound.
func (s *Store) ReadJSON(value any, parts ...string) error {
	body, err := os.ReadFile(s.Path(parts...))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}

	if err != nil {
		return fmt.Errorf("состояние не прочитано: %w", err)
	}

	err = json.Unmarshal(body, value)
	if err != nil {
		return fmt.Errorf("состояние не разобрано (%s): %w", s.Path(parts...), err)
	}

	return nil
}

// Open готовит корень состояния: каталоги службы с правами на владельца.
func Open(root string) (*Store, error) {
	dirs := []string{root, filepath.Join(root, "sessions"), filepath.Join(root, "inbox")}

	for _, dir := range dirs {
		err := os.MkdirAll(dir, DirPerm)
		if err != nil {
			return nil, fmt.Errorf("каталог состояния не создан: %w", err)
		}

		err = os.Chmod(dir, DirPerm)
		if err != nil {
			return nil, fmt.Errorf("права каталога состояния не выставлены: %w", err)
		}
	}

	return &Store{root: root}, nil
}

// WriteAtomic заменяет файл целиком: временный файл в том же каталоге плюс переименование.
func WriteAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)

	err := os.MkdirAll(dir, DirPerm)
	if err != nil {
		return fmt.Errorf("каталог состояния не создан: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("временный файл состояния не создан: %w", err)
	}

	name := tmp.Name()

	err = writeAndClose(tmp, body)
	if err != nil {
		_ = os.Remove(name)

		return err
	}

	err = os.Chmod(name, FilePerm)
	if err != nil {
		_ = os.Remove(name)

		return fmt.Errorf("права файла состояния не выставлены: %w", err)
	}

	err = os.Rename(name, path)
	if err != nil {
		_ = os.Remove(name)

		return fmt.Errorf("файл состояния не заменён: %w", err)
	}

	return nil
}

// writeAndClose пишет тело и закрывает файл, не оставляя недописанного содержимого.
func writeAndClose(file *os.File, body []byte) error {
	_, err := file.Write(body)
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("состояние не записано: %w", err)
	}

	err = file.Sync()
	if err != nil {
		_ = file.Close()

		return fmt.Errorf("состояние не сброшено на диск: %w", err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("файл состояния не закрыт: %w", err)
	}

	return nil
}
