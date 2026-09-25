//go:build windows

package models

import (
	"os/exec"
	"syscall"
)

// hideWindow 不为 Python 子进程弹出控制台窗口。
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
