//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

// decorate 让子进程成为新进程组组长，netx.KillTree 用负 PID 打整组。
func decorate(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
