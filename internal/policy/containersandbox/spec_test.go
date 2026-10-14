package containersandbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testImage(c Config) imageInfo {
	image := imageInfo{ID: c.Image}
	image.Config.Env = []string{"PATH=/image-path", "OPENAI_API_KEY=baked-secret", "NODE_OPTIONS=--require /loader"}
	image.Config.Labels = map[string]string{imageContractLabel: "1"}
	return image
}

func TestSandboxImageAndEnvironment(t *testing.T) {
	cfg := testConfig(t)
	image := testImage(cfg)
	spec, err := buildSpec(cfg, image, "/host/workspace", "sh", []string{"-c", "printf ok"}, []string{"SAFE_LABEL"}, []string{"SAFE_LABEL=reviewed", "OPENAI_API_KEY=service-secret", "DOCKER_HOST=tcp://host"}, "owned", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(spec.Env, "\n")
	if strings.Contains(env, "baked-secret") || strings.Contains(env, "service-secret") || strings.Contains(env, "--require") || !strings.Contains(env, "HOME=/workspace") || !strings.Contains(env, "SAFE_LABEL=reviewed") {
		t.Fatal("image or service environment leaked into sandbox")
	}
	if spec.HostConfig.NetworkMode != "none" || spec.HostConfig.MemorySwap != spec.HostConfig.Memory || spec.HostConfig.Mounts[0].Source != "/host/workspace" || !spec.HostConfig.Mounts[0].BindOptions.NonRecursive {
		t.Fatal("required network/resource/mount policy is missing")
	}
	image.Config.Volumes = map[string]any{"/unexpected": map[string]any{}}
	if _, err := buildSpec(cfg, image, "/host/workspace", "sh", nil, nil, nil, "owned", time.Second); err == nil {
		t.Fatal("image-declared volume accepted")
	}
}

func TestSandboxOutputFrames(t *testing.T) {
	frame := func(kind byte, value string) []byte {
		header := make([]byte, 8)
		header[0] = kind
		binary.BigEndian.PutUint32(header[4:], uint32(len(value)))
		return append(header, []byte(value)...)
	}
	var stdout, stderr bytes.Buffer
	if err := copyOutput(bytes.NewReader(append(frame(1, "stdout"), frame(2, "stderr")...)), &stdout, &stderr); err != nil || stdout.String() != "stdout" || stderr.String() != "stderr" {
		t.Fatal("Docker stream demultiplexing failed")
	}
	for _, bad := range [][]byte{frame(3, "bad"), {1, 0, 0}, frame(1, "missing")[:9]} {
		if copyOutput(bytes.NewReader(bad), io.Discard, io.Discard) == nil {
			t.Fatal("invalid output frame accepted")
		}
	}
	if sameEnv([]string{"KEY=bad", "KEY="}, []string{"KEY="}) {
		t.Fatal("duplicate environment entries passed verification")
	}
}

func TestSandboxEffectivePolicy(t *testing.T) {
	cfg := testConfig(t)
	spec, err := buildSpec(cfg, testImage(cfg), "/host/workspace", "sh", nil, nil, nil, "owned", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	original := infoForSpec(spec)
	if err := verifyCreated(original, spec); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*containerInfo)
	}{
		{"privileged", func(i *containerInfo) { i.HostConfig.Privileged = true }},
		{"network", func(i *containerInfo) { i.HostConfig.NetworkMode = "host" }},
		{"memory", func(i *containerInfo) { i.HostConfig.Memory = 0 }},
		{"oom disabled", func(i *containerInfo) { i.HostConfig.OOMKillDisable = true }},
		{"pids", func(i *containerInfo) { i.HostConfig.PidsLimit = 0 }},
		{"user", func(i *containerInfo) { i.Config.User = "0:0" }},
		{"image", func(i *containerInfo) { i.Image = "different" }},
		{"extra mount", func(i *containerInfo) { i.Mounts = append(i.Mounts, i.Mounts[0]) }},
		{"nonce", func(i *containerInfo) { i.Config.Labels = map[string]string{ownerLabel: "foreign"} }},
		{"secret env", func(i *containerInfo) {
			i.Config.Env = append(append([]string(nil), i.Config.Env...), "UNEXPECTED=secret")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			altered := original
			tc.mutate(&altered)
			if verifyCreated(altered, spec) == nil {
				t.Fatal("daemon policy drift accepted")
			}
		})
	}

}

func TestSandboxWorkspace(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkedWorkspace(context.Background(), resolved); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if checkedWorkspace(ctx, resolved) == nil {
		t.Fatal("canceled workspace scan continued")
	}
}

func infoForSpec(spec containerSpec) containerInfo {
	result := containerInfo{ID: strings.Repeat("b", 64), Image: spec.Image, Config: spec, HostConfig: spec.HostConfig}
	result.State.Status = "created"
	result.Mounts = append(result.Mounts, struct {
		Type, Source, Destination, Propagation string
		RW                                     bool
	}{Type: "bind", Source: spec.HostConfig.Mounts[0].Source, Destination: "/workspace", Propagation: "rprivate", RW: true})
	return result
}
