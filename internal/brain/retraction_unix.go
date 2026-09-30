//go:build !windows

package brain

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func readStableLedgerFile(project *secureDir, maxBytes int64, afterRead func()) ([]byte, error) {
	fd, err := unix.Openat(project.fd, "retractions.jsonl", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, errSecurePathMissing
	}
	if err != nil {
		return nil, fmt.Errorf("open brain retraction ledger: %w", ErrUnsafePath)
	}
	file := os.NewFile(uintptr(fd), "brain-retraction-ledger")
	defer file.Close()
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Size > maxBytes {
		return nil, fmt.Errorf("inspect brain retraction ledger: %w", ErrRetractionLedger)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(content)) > maxBytes || int64(len(content)) != before.Size {
		return nil, fmt.Errorf("read brain retraction ledger: %w", ErrRetractionLedger)
	}
	if afterRead != nil {
		afterRead()
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim {
		return nil, fmt.Errorf("brain retraction ledger changed during read: %w", ErrRetractionLedger)
	}
	return content, nil
}
