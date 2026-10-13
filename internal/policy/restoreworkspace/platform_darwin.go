//go:build darwin

package restoreworkspace

import (
	"fmt"
	"syscall"
)

func changeTime(s *syscall.Stat_t) string {
	return fmt.Sprintf("%d:%d", s.Ctimespec.Sec, s.Ctimespec.Nsec)
}
