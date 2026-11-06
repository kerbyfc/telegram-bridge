//go:build windows

package resolve

import (
	"errors"
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
// завершать задание, а не одну команду. Команда стартует приостановленной: в задание она попадает
// до первой своей инструкции, и ни один потомок мимо задания не рождается. Окна консоли команда
// не открывает: служба идёт без него.
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

	//nolint:exhaustruct // нужны одни флаги создания
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW}
	cmd.Cancel = func() error {
		return windows.TerminateJobObject(job, 1)
	}

	return &tree{job: job}, nil
}

// attach кладёт приостановленную команду в задание и пускает её главный поток: потомки команды
// наследуют задание сами.
func (t *tree) attach(cmd *exec.Cmd) error {
	pid := uint32(cmd.Process.Pid)

	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("процесс команды не открыт: %w", err)
	}
	defer windows.CloseHandle(proc) //nolint:errcheck // закрытие дескриптора исхода не меняет

	err = windows.AssignProcessToJobObject(t.job, proc)
	if err != nil {
		return fmt.Errorf("команда не положена в задание: %w", err)
	}

	return resumeProcess(pid)
}

// resumeProcess пускает потоки приостановленного процесса. Поток у только что созданного
// процесса один — главный; его номер даёт снимок потоков системы.
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("снимок потоков не снят: %w", err)
	}
	defer windows.CloseHandle(snap) //nolint:errcheck // закрытие дескриптора исхода не меняет

	entry := windows.ThreadEntry32{} //nolint:exhaustruct // заполняет система
	entry.Size = uint32(unsafe.Sizeof(entry))

	resumed := 0

	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}

		err = resumeThread(entry.ThreadID)
		if err != nil {
			return err
		}

		resumed++
	}

	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("потоки команды не перечислены: %w", err)
	}

	if resumed == 0 {
		return errors.New("у команды не нашлось потока")
	}

	return nil
}

// resumeThread пускает приостановленный поток.
func resumeThread(threadID uint32) error {
	thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, threadID)
	if err != nil {
		return fmt.Errorf("поток команды не открыт: %w", err)
	}
	defer windows.CloseHandle(thread) //nolint:errcheck // закрытие дескриптора исхода не меняет

	_, err = windows.ResumeThread(thread)
	if err != nil {
		return fmt.Errorf("поток команды не пущен: %w", err)
	}

	return nil
}

// release закрывает задание: потомки, пережившие команду, умирают вместе с ним.
func (t *tree) release() {
	_ = windows.CloseHandle(t.job)
}
