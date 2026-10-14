package containersandbox

import (
	"errors"
	"reflect"
	"strings"
)

func verifyCreated(info containerInfo, want containerSpec) error {
	h := info.HostConfig
	w := want.HostConfig
	if !validContainerID(info.ID) || info.Image != want.Image || info.Config.Image != want.Image || info.Config.User != want.User || info.Config.WorkingDir != "/workspace" || info.Config.Tty || info.Config.OpenStdin || !reflect.DeepEqual(info.Config.Entrypoint, want.Entrypoint) || !reflect.DeepEqual(info.Config.Cmd, want.Cmd) || info.Config.Labels[ownerLabel] != want.Labels[ownerLabel] || !sameEnv(info.Config.Env, want.Env) || !reflect.DeepEqual(info.Config.Healthcheck.Test, []string{"NONE"}) {
		return errors.New("sandbox effective command, identity or environment does not match the required policy")
	}
	nnp := len(h.SecurityOpt) == 1 && (h.SecurityOpt[0] == "no-new-privileges:true" || h.SecurityOpt[0] == "no-new-privileges")
	if h.NetworkMode != "none" || !h.ReadonlyRootfs || h.Privileged || h.OOMKillDisable || !h.Init || h.Runtime != "runc" || h.PidMode != "" || h.IpcMode != "private" || h.UTSMode != "" || h.CgroupnsMode != "private" || h.UsernsMode != "" || !nnp || len(h.CapAdd) != 0 || len(h.CapDrop) != 1 || strings.ToUpper(h.CapDrop[0]) != "ALL" || h.NanoCpus != w.NanoCpus || h.Memory != w.Memory || h.MemorySwap != w.MemorySwap || h.PidsLimit != w.PidsLimit || h.ShmSize != w.ShmSize || !reflect.DeepEqual(h.Tmpfs, w.Tmpfs) || len(h.Binds) != 0 || len(h.VolumesFrom) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || len(h.PortBindings) != 0 || h.PublishAllPorts || h.RestartPolicy.Name != "no" || h.RestartPolicy.MaximumRetryCount != 0 || h.LogConfig.Type != "none" {
		return errors.New("sandbox effective isolation or resource limits do not match the required policy")
	}
	if len(h.Mounts) != 1 || !reflect.DeepEqual(h.Mounts, w.Mounts) {
		return errors.New("sandbox effective mount configuration does not match the required policy")
	}
	binds := 0
	for _, m := range info.Mounts {
		if m.Type == "tmpfs" && m.Destination == "/tmp" && m.Source == "" {
			continue
		}
		if m.Type != "bind" || m.Source != w.Mounts[0].Source || m.Destination != "/workspace" || !m.RW || m.Propagation != "rprivate" {
			return errors.New("sandbox contains an unexpected effective mount")
		}
		binds++
	}
	if binds != 1 {
		return errors.New("sandbox workspace mount is missing")
	}
	return nil
}

func sameEnv(a, b []string) bool {
	toMap := func(items []string) map[string]string {
		values := map[string]string{}
		for _, entry := range items {
			key, value, ok := strings.Cut(entry, "=")
			if !ok {
				return nil
			}
			if _, duplicate := values[key]; duplicate {
				return nil
			}
			values[key] = value
		}
		return values
	}
	left, right := toMap(a), toMap(b)
	return left != nil && right != nil && reflect.DeepEqual(left, right)
}
