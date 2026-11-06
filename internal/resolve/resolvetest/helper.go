// Package resolvetest — поддельная команда разворота для тестов: сам тестовый бинарь, запущенный
// с особым первым аргументом. Оболочка и скрипты не нужны, поэтому тесты идут одинаково на macOS и
// Windows.
package resolvetest

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// marker — первый аргумент, которым тестовый бинарь узнаёт, что он поддельная команда.
const marker = "resolver-helper"

// Режимы поддельной команды.
const (
	// ModePrint печатает ссылку с переводами строк Windows.
	ModePrint = "print"
	// ModeFail пишет причину в поток ошибок и выходит с кодом 3.
	ModeFail = "fail"
	// ModeSleep спит дольше любого срока теста.
	ModeSleep = "sleep"
	// ModeSpawn запускает спящего потомка, пишет его pid в файл и спит сама.
	ModeSpawn = "spawn"
	// ModeEmpty ничего не выводит.
	ModeEmpty = "empty"
	// ModeBadUTF8 выводит байты не в UTF-8.
	ModeBadUTF8 = "bad-utf8"
	// ModeSlow спит SlowPause и печатает ссылку, как ModePrint.
	ModeSlow = "slow"
	// ModeFlood пишет вывод без конца.
	ModeFlood = "flood"
	// ModeOrphan печатает ссылку и выходит, оставив потомка, который держит поток вывода.
	ModeOrphan = "orphan"
)

// SlowPause — сколько думает команда режима ModeSlow.
const SlowPause = 300 * time.Millisecond

// FailExit — код выхода режима ModeFail.
const FailExit = 3

// Serve исполняет поддельную команду, если бинарь запущен ею, и выходит; иначе возвращается.
// Зовётся первой строкой `TestMain`.
func Serve() {
	if len(os.Args) < 3 || os.Args[1] != marker {
		return
	}

	os.Exit(run(os.Args[2], os.Args[3:]))
}

// Command собирает команду записи разворота: тестовый бинарь, режим и аргументы.
func Command(mode string, args ...string) []string {
	return append([]string{os.Args[0], marker, mode}, args...)
}

func run(mode string, args []string) int {
	switch mode {
	case ModePrint:
		fmt.Fprintf(os.Stdout, "развёрнуто %s\r\nвторая строка\r\n", args[0])
	case ModeFail:
		fmt.Fprintln(os.Stderr, "нет такой заметки")

		return FailExit
	case ModeSleep:
		time.Sleep(time.Minute)
	case ModeSpawn:
		child := exec.Command(os.Args[0], marker, ModeSleep) //nolint:gosec,noctx // тестовая команда
		child.Stdout = os.Stdout

		if child.Start() != nil {
			return 1
		}

		_ = os.WriteFile(args[0], []byte(strconv.Itoa(child.Process.Pid)), 0o600)

		time.Sleep(time.Minute)
	case ModeSlow:
		time.Sleep(SlowPause)
		fmt.Fprintf(os.Stdout, "развёрнуто %s\r\nвторая строка\r\n", args[0])
	case ModeFlood:
		chunk := make([]byte, 64<<10)
		for i := range chunk {
			chunk[i] = 'a'
		}

		for {
			_, err := os.Stdout.Write(chunk)
			if err != nil {
				return 1
			}
		}
	case ModeOrphan:
		child := exec.Command(os.Args[0], marker, ModeSleep) //nolint:gosec,noctx // тестовая команда
		child.Stdout = os.Stdout

		if child.Start() != nil {
			return 1
		}

		fmt.Fprintf(os.Stdout, "развёрнуто %s\n", args[0])
	case ModeEmpty:
	case ModeBadUTF8:
		_, _ = os.Stdout.Write([]byte{0xff, 0xfe, '\n'})
	default:
		return 2
	}

	return 0
}
