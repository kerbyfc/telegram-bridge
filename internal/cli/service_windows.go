//go:build windows

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

var (
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	user32               = windows.NewLazySystemDLL("user32.dll")
	procFreeConsole      = kernel32.NewProc("FreeConsole")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
)

// hideServiceWindow убирает консольное окно службы: задача планировщика запускает службу с
// InteractiveToken, и без этого окно консоли появляется на рабочем столе при каждом входе.
// Закрытие окна владельцем убивает службу, поэтому она его не показывает. Сначала `FreeConsole`
// отцепляет процесс от консоли — окно закрывается целиком; если отцепление не удалось, окно
// скрывается `ShowWindow`.
func hideServiceWindow() {
	detached, _, _ := procFreeConsole.Call()
	if detached != 0 {
		return
	}

	console, _, _ := procGetConsoleWindow.Call()
	if console == 0 {
		return
	}

	showWindow(console, windows.SW_HIDE)
}

// showWindow — WinAPI `ShowWindow` с типами вместо сырых регистров вызова: ошибки у функции нет,
// её ответ — было ли окно видно до вызова.
func showWindow(window uintptr, command int32) (wasVisible bool) {
	visible, _, _ := procShowWindow.Call(window, uintptr(command))

	return visible != 0
}

// redirectServiceOutput направляет стандартную выдачу и поток ошибок службы в файл вывода: у
// отвязанного процесса нет терминала, а причина отказа обязана быть видна самопроверке и владельцу.
// Файл остаётся открытым до конца процесса: строку об отказе печатает тот же процесс уже после
// возврата из службы. Панику пишет сама среда исполнения мимо этих потоков — её в файле нет.
func redirectServiceOutput(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("вывод службы не перенаправлен: %w", err)
	}

	os.Stdout = file
	os.Stderr = file

	return nil
}
