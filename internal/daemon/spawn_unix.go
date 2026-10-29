//go:build !windows

package daemon

import (
	"os/exec"
	"syscall"
)

// decorateSpawn 让 daemon 脱离会话（Setsid），成为一个独立进程。
func decorateSpawn(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
