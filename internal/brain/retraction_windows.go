//go:build windows

package brain

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func readStableLedgerFile(project *secureDir, maxBytes int64, afterRead func()) ([]byte, error) {
	if project == nil || project.path == "" {
		return nil, ErrUnsafePath
	}
	path := filepath.Join(project.path, "retractions.jsonl")
	pathp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("open brain retraction ledger: %w", ErrUnsafePath)
	}
	handle, err := windows.CreateFile(
		pathp,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil, errSecurePathMissing
	}
	if err != nil {
		return nil, fmt.Errorf("open brain retraction ledger: %w", ErrUnsafePath)
	}
	file := os.NewFile(uintptr(handle), "brain-retraction-ledger")
	defer file.Close()

	before, err := retractionFileInformation(handle)
	if err != nil || before.attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || before.attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || before.size > maxBytes {
		return nil, fmt.Errorf("inspect brain retraction ledger: %w", ErrRetractionLedger)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(content)) > maxBytes || int64(len(content)) != before.size {
		return nil, fmt.Errorf("read brain retraction ledger: %w", ErrRetractionLedger)
	}
	if afterRead != nil {
		afterRead()
	}
	after, err := retractionFileInformation(handle)
	if err != nil || before != after {
		return nil, fmt.Errorf("brain retraction ledger changed during read: %w", ErrRetractionLedger)
	}
	return content, nil
}

type retractionFileInfo struct {
	volumeSerialNumber uint32
	fileIndexHigh      uint32
	fileIndexLow       uint32
	lastWriteTimeHigh  uint32
	lastWriteTimeLow   uint32
	size               int64
	attributes         uint32
}

func retractionFileInformation(handle windows.Handle) (retractionFileInfo, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return retractionFileInfo{}, err
	}
	size := int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow)
	return retractionFileInfo{
		volumeSerialNumber: info.VolumeSerialNumber,
		fileIndexHigh:      info.FileIndexHigh,
		fileIndexLow:       info.FileIndexLow,
		lastWriteTimeHigh:  info.LastWriteTime.HighDateTime,
		lastWriteTimeLow:   info.LastWriteTime.LowDateTime,
		size:               size,
		attributes:         info.FileAttributes,
	}, nil
}
