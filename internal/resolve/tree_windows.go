//go:build windows

package resolve

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// tree — команда со всеми потомками: объект задания, который срок закрывает целиком.
type tree struct {
	job windows.Handle
}

// newTree заводит объект задания, убивающий все процессы при закрытии, и учит отмену
// завершать задание, а не одну команду. Окна консоли команда не открывает: служба идёт без него.
func newTree(cmd *exec.Cmd) (*tree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("объект задания не создан: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{} //nolint:exhaustruct // нужен один флаг
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE

	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		_ = windows.CloseHandle(job)

		return nil, fmt.Errorf("объект задания не настроен: %w", err)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW} //nolint:exhaustruct // нужен один флаг
	cmd.Cancel = func() error {
		return windows.TerminateJobObject(job, 1)
	}

	return &tree{job: job}, nil
}

// attach кладёт запущенную команду в задание: её потомки наследуют его сами.
func (t *tree) attach(cmd *exec.Cmd) error {
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("процесс команды не открыт: %w", err)
	}
	defer windows.CloseHandle(proc) //nolint:errcheck // закрытие дескриптора исхода не меняет

	err = windows.AssignProcessToJobObject(t.job, proc)
	if err != nil {
		return fmt.Errorf("команда не положена в задание: %w", err)
	}

	return nil
}

// release закрывает задание: потомки, пережившие команду, умирают вместе с ним.
func (t *tree) release() {
	_ = windows.CloseHandle(t.job)
}
