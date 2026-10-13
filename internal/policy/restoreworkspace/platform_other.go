//go:build !darwin && !linux

package restoreworkspace

import "os"

func supported() bool                                { return false }
func singleLink(os.FileInfo) bool                    { return false }
func fileVersion(os.FileInfo) string                 { return "unsupported" }
func openRegular(*os.Root, string) (*os.File, error) { return nil, ErrUnsupported }
