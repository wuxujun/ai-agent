package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestToolSandboxDefaultsAndRestart(t *testing.T) {
	path, _ := loadBrainReloadFixture(t)
	before, revision := Get(), Revision()
	if before.Tool.Sandbox.Mode != "local" || before.Tool.Sandbox.User != "65532:65532" || before.Tool.Sandbox.MemoryMiB != 256 {
		t.Fatal("sandbox defaults incorrect")
	}
	body := append(brainReloadConfig("./data/brain"), []byte("\ntool:\n  sandbox:\n    memory_mib: 512\n")...)
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reload(); !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("sandbox changed without restart: %v", err)
	}
	if Get() != before || Revision() != revision {
		t.Fatal("rejected reload replaced active snapshot")
	}
	body = append(brainReloadConfig("./data/brain"), []byte("\ntool:\n  sandbox:\n    mode: untrusted\n")...)
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reload(); err == nil {
		t.Fatal("invalid sandbox accepted")
	}
	if Get() != before || Revision() != revision {
		t.Fatal("invalid reload replaced active snapshot")
	}
	body = append(brainReloadConfig("./data/brain"), []byte("\ntool:\n  timeout_seconds: 15\n")...)
	if err := os.WriteFile(path, body, 0644); err != nil {
		t.Fatal(err)
	}
	if _, changes, err := Reload(); err != nil || !strings.Contains(strings.Join(changes, "\n"), "timeout_seconds") {
		t.Fatalf("compatible local reload rejected: %v", err)
	}
}
