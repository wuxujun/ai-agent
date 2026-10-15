//go:build !windows

package tools

import "syscall"

const readFileNonblock = syscall.O_NONBLOCK
