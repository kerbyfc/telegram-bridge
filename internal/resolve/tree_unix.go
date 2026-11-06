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
//
// Группа названа номером команды, которой уже нет. Номер группы система не отдаёт новому
// процессу, пока в группе жив хоть один процесс, поэтому живая группа здесь всегда своя. Узкое
// окно остаётся одно: группа опустела, а номер успел уйти новому лидеру своей группы — сигнал
// тогда попал бы в чужую группу. Для этого система должна обойти весь круг номеров за время между
// концом команды и этой строкой.
//
// Потомок, ушедший в свой сеанс (`setsid`) или свою группу, из группы выпадает, и ни срок, ни
// эта строка его не достают.
func (t *tree) release() {
	if t.cmd.Process == nil {
		return
	}

	_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
}
