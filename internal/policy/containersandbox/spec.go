package containersandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/policy/commandenv"
)

const imageContractLabel = "ai-agent.execution-image.version"
const ownerLabel = "ai-agent.execution.nonce"
const apiVersion = "/v1.45"
const controlTimeout = 5 * time.Second
const cleanupTimeout = 10 * time.Second

// ExitError describes a process exit, rather than an infrastructure failure.
// Its exit-code contract is shared with os/exec.ExitError by the tool layer.
type ExitError struct {
	Code      int
	OOMKilled bool
}

func (e *ExitError) Error() string {
	if e.OOMKilled {
		return "sandbox command exceeded its memory limit"
	}
	return fmt.Sprintf("sandbox command exited with code %d", e.Code)
}
func (e *ExitError) ExitCode() int { return e.Code }

var ErrCleanup = errors.New("sandbox container cleanup could not be confirmed")

type imageInfo struct {
	ID     string `json:"Id"`
	Config struct {
		Env     []string
		Volumes map[string]any
		Labels  map[string]string
	}
}

type mount struct {
	Type        string
	Source      string
	Target      string
	ReadOnly    bool
	BindOptions struct {
		Propagation  string
		NonRecursive bool
	}
}

type hostConfig struct {
	NetworkMode     string
	ReadonlyRootfs  bool
	CapDrop         []string
	CapAdd          []string
	SecurityOpt     []string
	Privileged      bool
	OOMKillDisable  bool `json:"OomKillDisable"`
	Init            bool
	Runtime         string
	PidMode         string
	IpcMode         string
	UTSMode         string
	CgroupnsMode    string
	UsernsMode      string
	NanoCpus        int64
	Memory          int64
	MemorySwap      int64
	PidsLimit       int64
	ShmSize         int64
	Tmpfs           map[string]string
	Mounts          []mount
	Binds           []string
	VolumesFrom     []string
	Devices         []any
	DeviceRequests  []any
	PortBindings    map[string]any
	PublishAllPorts bool
	RestartPolicy   struct {
		Name              string
		MaximumRetryCount int
	}
	LogConfig struct {
		Type   string
		Config map[string]string
	}
}

type containerSpec struct {
	Image        string
	User         string
	WorkingDir   string
	Entrypoint   []string
	Cmd          []string
	Env          []string
	Labels       map[string]string
	Tty          bool
	OpenStdin    bool
	AttachStdout bool
	AttachStderr bool
	Healthcheck  struct{ Test []string }
	HostConfig   hostConfig
}

type containerInfo struct {
	ID         string `json:"Id"`
	Image      string
	Config     containerSpec
	HostConfig hostConfig
	Mounts     []struct {
		Type, Source, Destination, Propagation string
		RW                                     bool
	}
	State struct {
		Status    string
		Running   bool
		ExitCode  int
		OOMKilled bool
		Error     string
	}
}

func checkedWorkspace(ctx context.Context, workspace string) error {
	info, err := os.Lstat(workspace)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("sandbox workspace must be an existing directory")
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil || resolved != workspace {
		return errors.New("sandbox workspace must not traverse symlinks")
	}
	// Host sockets, FIFOs, and devices would bridge the mount/network boundary.
	// Do not follow workspace symlinks: they resolve inside the container.
	count := 0
	err = filepath.WalkDir(workspace, func(_ string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return errors.New("sandbox workspace inspection failed")
		}
		count++
		if count > 100000 {
			return errors.New("sandbox workspace exceeds the safe inspection bound")
		}
		if entry.Type()&(os.ModeSocket|os.ModeNamedPipe|os.ModeDevice|os.ModeCharDevice) != 0 {
			return errors.New("sandbox workspace contains a host socket, FIFO or device")
		}
		return nil
	})
	return err
}

func buildSpec(c Config, image imageInfo, workspace, name string, args []string, extra, hostEnv []string, nonce string, duration time.Duration) (containerSpec, error) {
	if image.ID != c.Image || image.Config.Labels[imageContractLabel] != "1" || len(image.Config.Volumes) != 0 {
		return containerSpec{}, errors.New("sandbox image is not the reviewed immutable execution image or declares extra volumes")
	}
	if err := commandenv.ValidateAllowlist(extra); err != nil {
		return containerSpec{}, err
	}
	values := map[string]string{}
	for _, entry := range image.Config.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsAny(key, "\x00\r\n") {
			return containerSpec{}, errors.New("sandbox image environment is invalid")
		}
		values[key] = "" // Docker otherwise merges baked-in image variables back in.
	}
	allowed := map[string]bool{}
	for _, key := range extra {
		allowed[key] = true
	}
	for _, entry := range hostEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok && allowed[key] && !strings.ContainsRune(value, '\x00') {
			values[key] = value
		}
	}
	// Host paths and client settings never select the container's executable,
	// home or caches. Non-inherited image variables are explicitly cleared.
	for key, value := range map[string]string{"PATH": "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME": "/workspace", "USERPROFILE": "/workspace", "PWD": "/workspace", "TMPDIR": "/tmp", "TMP": "/tmp", "TEMP": "/tmp", "LANG": "C.UTF-8", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GOCACHE": "/tmp/go-cache", "GOMODCACHE": "/tmp/go-mod-cache", "GOPATH": "/tmp/go", "GOROOT": "/usr/local/go", "GOTOOLCHAIN": "local"} {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(values))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	seconds := int64(duration/time.Second) + 1
	if seconds < 1 || seconds > 3601 {
		return containerSpec{}, errors.New("sandbox watchdog must be bounded to one hour")
	}
	command := []string{"--signal=KILL", "--kill-after=1s", "--", strconv.FormatInt(seconds, 10) + "s", name}
	command = append(command, args...)
	spec := containerSpec{Image: image.ID, User: c.User, WorkingDir: "/workspace", Entrypoint: []string{"/usr/bin/timeout"}, Cmd: command, Env: environment, Labels: map[string]string{ownerLabel: nonce}, AttachStdout: true, AttachStderr: true}
	spec.Healthcheck.Test = []string{"NONE"}
	h := &spec.HostConfig
	h.NetworkMode = "none"
	h.ReadonlyRootfs = true
	h.CapDrop = []string{"ALL"}
	h.SecurityOpt = []string{"no-new-privileges:true"}
	h.Init = true
	h.Runtime = "runc"
	h.IpcMode = "private"
	h.CgroupnsMode = "private"
	h.NanoCpus = int64(c.CPUs * 1e9)
	h.Memory = c.MemoryMiB << 20
	h.MemorySwap = h.Memory
	h.PidsLimit = c.PidsLimit
	h.ShmSize = 16 << 20
	h.Tmpfs = map[string]string{"/tmp": fmt.Sprintf("rw,exec,nosuid,nodev,size=%dm,mode=1777", c.TmpfsMiB)}
	m := mount{Type: "bind", Source: workspace, Target: "/workspace"}
	m.BindOptions.Propagation = "rprivate"
	m.BindOptions.NonRecursive = true
	h.Mounts = []mount{m}
	h.RestartPolicy.Name = "no"
	h.LogConfig.Type = "none"
	return spec, nil
}
