//go:build darwin || linux

package restoreworkspace

import (
	"fmt"
	"os"
	"syscall"
)

func supported() bool { return true }
func singleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
func fileVersion(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "unsupported"
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%s", stat.Dev, stat.Ino, stat.Nlink, info.Size(), info.Mode(), info.ModTime().UnixNano(), changeTime(stat))
}
func openRegular(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
