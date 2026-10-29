//go:build windows

package proc

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// decorate 让子进程独立成组，配合 taskkill /T 一并结束整棵树。
// 注意：分离出来的子进程没有控制台，因此 Spring Boot 的优雅关闭（@PreDestroy、
// 连接池释放）在这条路上做不到 —— 只能强杀 + 事后核对端口是否真的释放。
// 这是平台限制，不把它包装成"已优雅停止"。
func decorate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | createNoWindow}
}
