//go:build !windows

package policy

import "syscall"

// O_NOFOLLOW is the platform-specific flag to prevent following symlinks.
const O_NOFOLLOW = syscall.O_NOFOLLOW
