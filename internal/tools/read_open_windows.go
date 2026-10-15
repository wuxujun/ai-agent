//go:build windows

package tools

// Windows has no Unix FIFO open semantics; Root still confines path traversal.
const readFileNonblock = 0
