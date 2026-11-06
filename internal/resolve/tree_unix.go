//go:build !windows

package resolve

import (
	"os/exec"
	"syscall"
)

// tree — команда со всеми потомками: своя группа процессов, которую срок убивает целиком.
type tree struct {
	cmd *exec.Cmd
}

// newTree заводит команде свою группу процессов и учит отмену убивать группу, а не одну команду.
func newTree(cmd *exec.Cmd) (*tree, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	return &tree{cmd: cmd}, nil
}

// attach ничего не делает: группа заведена при запуске.
func (t *tree) attach(_ *exec.Cmd) error {
	return nil
}

// release добивает потомков, переживших команду: висящих процессов разворот не оставляет.
func (t *tree) release() {
	if t.cmd.Process == nil {
		return
	}

	_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
}
