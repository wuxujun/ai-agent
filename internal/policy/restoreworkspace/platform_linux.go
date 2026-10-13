//go:build linux

package restoreworkspace

import (
	"fmt"
	"syscall"
)

func changeTime(s *syscall.Stat_t) string { return fmt.Sprintf("%d:%d", s.Ctim.Sec, s.Ctim.Nsec) }
