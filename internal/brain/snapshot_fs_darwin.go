//go:build darwin

package brain

import "golang.org/x/sys/unix"

func renameNoReplaceAt(fromFD int, from string, toFD int, to string) error {
	return unix.RenameatxNp(fromFD, from, toFD, to, unix.RENAME_EXCL)
}
