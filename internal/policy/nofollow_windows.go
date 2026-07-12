//go:build windows

package policy

// O_NOFOLLOW is defined as 0 on Windows since Windows does not support this open flag natively.
const O_NOFOLLOW = 0
