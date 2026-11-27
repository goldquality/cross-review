//go:build !windows

package client

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree убивает группу процессов команды: ./dev review запускает docker, claude и прочих детей.
func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	go func(pid int) {
		// Дать детям две секунды закончить, потом добить.
		<-afterSeconds(2)
		syscall.Kill(-pid, syscall.SIGKILL)
	}(cmd.Process.Pid)
}
