//go:build !windows

package models

import "os/exec"

func hideWindow(*exec.Cmd) {}
