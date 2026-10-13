// Package restoreworkspace reads only tenant/task roots inside a dedicated
// restore sandbox. It does not use the runtime workspace or online config.
package restoreworkspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const MaxFiles = 1024
const MaxBytes int64 = 64 << 20
const maxEntries = 4096
const maxDepth = 32

var ErrMissing = errors.New("restore workspace missing")
var ErrUnsafe = errors.New("restore workspace unsafe")
var ErrBudget = errors.New("restore workspace exceeds budget")
var ErrChanged = errors.New("restore workspace changed during read")
var ErrUnsupported = errors.New("restore workspace platform unsupported")

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

type Snapshot struct {
	Files    []File
	Revision string
}

func digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func RelativeRoot(task, tenant string) string {
	return filepath.Join("workspaces", digest([]byte(tenant)), digest([]byte(task)))
}

func ValidPath(name string) bool {
	if name == "." || len(name) == 0 || len(name) > 512 || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:") {
		return false
	}
	for _, c := range name {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func private(info os.FileInfo, dir bool) bool {
	return info.Mode().Perm()&0077 == 0 && info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && (dir && info.IsDir() || !dir && info.Mode().IsRegular())
}

// openDirectory pins a directory and checks its identity on both sides of the
// open. Every traversal step is relative to an already-pinned parent root.
func openDirectory(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrMissing
		}
		return nil, ErrUnsafe
	}
	if !private(before, true) {
		return nil, ErrUnsafe
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, ErrUnsafe
	}
	opened, err := child.Stat(".")
	after, afterErr := parent.Lstat(name)
	if err != nil || afterErr != nil || !os.SameFile(before, opened) || !os.SameFile(before, after) || !private(after, true) {
		child.Close()
		return nil, ErrChanged
	}
	return child, nil
}

type scanner struct {
	ctx      context.Context
	files    []File
	entries  int
	bytes    int64
	versions []string
	buffer   []byte
}

func (s *scanner) readFile(root *os.Root, name, path string, before os.FileInfo) error {
	if !private(before, false) || !singleLink(before) {
		return ErrUnsafe
	}
	if len(s.files) >= MaxFiles || before.Size() < 0 || before.Size() > MaxBytes-s.bytes {
		return ErrBudget
	}
	file, err := openRegular(root, name)
	if err != nil {
		return ErrUnsafe
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !private(opened, false) || !singleLink(opened) || !os.SameFile(before, opened) || fileVersion(before) != fileVersion(opened) {
		return ErrChanged
	}
	hash := sha256.New()
	var total int64
	if s.buffer == nil {
		s.buffer = make([]byte, 32<<10)
	}
	buffer := s.buffer
	for {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		n, err := file.Read(buffer)
		total += int64(n)
		if total > MaxBytes-s.bytes {
			return ErrBudget
		}
		if total > opened.Size() {
			return ErrChanged
		}
		hash.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrUnsafe
		}
	}
	after, err := file.Stat()
	current, currentErr := root.Lstat(name)
	if err != nil || currentErr != nil || !os.SameFile(opened, current) || !private(current, false) || !singleLink(current) || fileVersion(opened) != fileVersion(after) || fileVersion(opened) != fileVersion(current) || total != opened.Size() {
		return ErrChanged
	}
	s.bytes += total
	s.files = append(s.files, File{Path: path, Size: total, SHA256: hex.EncodeToString(hash.Sum(nil)), Mode: uint32(opened.Mode().Perm())})
	s.versions = append(s.versions, path+":"+fileVersion(opened))
	return nil
}

func (s *scanner) walk(root *os.Root, prefix string, depth int) error {
	if depth > maxDepth {
		return ErrBudget
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	before, err := root.Stat(".")
	if err != nil || !private(before, true) {
		return ErrUnsafe
	}
	dir, err := root.Open(".")
	if err != nil {
		return ErrUnsafe
	}
	defer dir.Close()
	for {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(64)
		for _, entry := range entries {
			s.entries++
			if s.entries > maxEntries {
				return ErrBudget
			}
			name := entry.Name()
			path := name
			if prefix != "" {
				path = prefix + "/" + name
			}
			if !ValidPath(path) {
				return ErrUnsafe
			}
			info, err := root.Lstat(name)
			if err != nil {
				return ErrChanged
			}
			if info.IsDir() {
				child, err := openDirectory(root, name)
				if err != nil {
					return err
				}
				err = s.walk(child, path, depth+1)
				child.Close()
				if err != nil {
					return err
				}
			} else if err := s.readFile(root, name, path, info); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return ErrUnsafe
		}
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) || fileVersion(before) != fileVersion(after) {
		return ErrChanged
	}
	s.versions = append(s.versions, prefix+"/:"+fileVersion(after))
	return nil
}

// Scan never follows a manifest-provided path. sandbox must already have been
// validated as a dedicated SQLite restore sandbox by its trusted caller. No
// symlinks, hard-linked files or special files are read; reads are bounded and
// cancellable. Access times may be changed by the OS; no content is written.
func Scan(ctx context.Context, sandbox, task, tenant string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if !supported() {
		return Snapshot{}, ErrUnsupported
	}
	if task == "" || tenant == "" {
		return Snapshot{}, ErrUnsafe
	}
	absolute, err := filepath.Abs(sandbox)
	if err != nil {
		return Snapshot{}, ErrUnsafe
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil || canonical != absolute {
		return Snapshot{}, ErrUnsafe
	}
	before, err := os.Lstat(absolute)
	if err != nil || !private(before, true) {
		return Snapshot{}, ErrUnsafe
	}
	parent, err := os.OpenRoot(absolute)
	if err != nil {
		return Snapshot{}, ErrUnsafe
	}
	defer parent.Close()
	opened, err := parent.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		return Snapshot{}, ErrChanged
	}
	root := parent
	var roots []*os.Root
	defer func() {
		for _, r := range roots {
			r.Close()
		}
	}()
	for _, component := range strings.Split(filepath.ToSlash(RelativeRoot(task, tenant)), "/") {
		child, err := openDirectory(root, component)
		if err != nil {
			return Snapshot{}, err
		}
		roots = append(roots, child)
		root = child
	}
	s := scanner{ctx: ctx, files: []File{}}
	if err := s.walk(root, "", 0); err != nil {
		return Snapshot{}, err
	}
	// Recheck named roots as well as the pinned descriptors after reading.
	check := parent
	for i, component := range strings.Split(filepath.ToSlash(RelativeRoot(task, tenant)), "/") {
		named, err := check.Lstat(component)
		pinned, pinnedErr := roots[i].Stat(".")
		if err != nil || pinnedErr != nil || !private(named, true) || !os.SameFile(named, pinned) {
			return Snapshot{}, ErrChanged
		}
		s.versions = append(s.versions, component+":"+fileVersion(pinned))
		check = roots[i]
	}
	current, err := os.Lstat(absolute)
	if err != nil || !private(current, true) || !os.SameFile(before, current) {
		return Snapshot{}, ErrChanged
	}
	sort.Slice(s.files, func(i, j int) bool { return s.files[i].Path < s.files[j].Path })
	sort.Strings(s.versions)
	encoded, _ := json.Marshal(struct {
		Files    []File
		Versions []string
	}{s.files, s.versions})
	return Snapshot{Files: s.files, Revision: digest(encoded)}, nil
}
