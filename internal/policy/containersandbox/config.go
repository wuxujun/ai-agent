// Package containersandbox executes approved commands in bounded Linux
// containers through one explicitly configured local Docker Unix socket.
package containersandbox

import (
	"encoding/hex"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Mode       string  `mapstructure:"mode"`
	SocketPath string  `mapstructure:"socket_path"`
	Image      string  `mapstructure:"image"`
	User       string  `mapstructure:"user"`
	CPUs       float64 `mapstructure:"cpus"`
	MemoryMiB  int64   `mapstructure:"memory_mib"`
	PidsLimit  int64   `mapstructure:"pids_limit"`
	TmpfsMiB   int64   `mapstructure:"tmpfs_mib"`
}

func (c Config) Docker() bool { return c.Mode == "docker" }

func (c Config) Validate() error {
	if math.IsNaN(c.CPUs) || math.IsInf(c.CPUs, 0) || c.CPUs < 0 || c.MemoryMiB < 0 || c.PidsLimit < 0 || c.TmpfsMiB < 0 {
		return errors.New("tool.sandbox resource values must be finite and nonnegative")
	}
	if c.Mode == "" || c.Mode == "local" {
		return nil
	}
	if c.Mode != "docker" {
		return errors.New("tool.sandbox.mode must be local or docker")
	}
	if !filepath.IsAbs(c.SocketPath) || strings.ContainsRune(c.SocketPath, '\x00') || c.SocketPath != filepath.Clean(c.SocketPath) {
		return errors.New("tool.sandbox.socket_path must be an absolute local Unix socket path")
	}
	if !validID(c.Image) {
		return errors.New("tool.sandbox.image must be a complete immutable sha256 image ID already present in the local daemon")
	}
	parts := strings.Split(c.User, ":")
	if len(parts) != 2 {
		return errors.New("tool.sandbox.user must be a non-root numeric UID:GID")
	}
	for _, part := range parts {
		id, err := strconv.ParseUint(part, 10, 32)
		if err != nil || id == 0 || id == (1<<32)-1 || part != strconv.FormatUint(id, 10) {
			return errors.New("tool.sandbox.user must be a non-root numeric UID:GID")
		}
	}
	if math.IsNaN(c.CPUs) || math.IsInf(c.CPUs, 0) || c.CPUs < 0.01 || c.CPUs > 64 {
		return errors.New("tool.sandbox.cpus must be between 0.01 and 64")
	}
	if c.MemoryMiB < 16 || c.MemoryMiB > 32768 || c.PidsLimit < 8 || c.PidsLimit > 4096 || c.TmpfsMiB < 1 || c.TmpfsMiB > c.MemoryMiB {
		return errors.New("tool.sandbox memory, process and tmpfs limits must be positive and within supported bounds")
	}
	return nil
}

func validID(id string) bool {
	if !strings.HasPrefix(id, "sha256:") {
		return false
	}
	return validContainerID(strings.TrimPrefix(id, "sha256:"))
}

func validContainerID(id string) bool {
	_, err := hex.DecodeString(id)
	return err == nil && len(id) == 64 && strings.ToLower(id) == id
}
