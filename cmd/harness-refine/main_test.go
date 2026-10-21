package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelpComposeExampleRuns(t *testing.T) {
	var help bytes.Buffer
	if code := run([]string{"--help"}, &help, io.Discard); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	// Execute the published example with real local probe/snapshot fixtures,
	// so missing required arguments in the help text cause a regression.
	flat := strings.ReplaceAll(help.String(), "\\\n", " ")
	for _, line := range strings.Split(flat, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "harness-refine" || fields[1] != "compose" {
			continue
		}
		out := t.TempDir()
		writeJSON(t, filepath.Join(out, "probe-results.json"), testReport())
		writeJSON(t, filepath.Join(out, "base-snapshot.json"), testBaseSnapshot(t))
		args := strings.Fields(strings.ReplaceAll(strings.TrimSpace(line), "runs/hr/<ts>", out))
		var stderr bytes.Buffer
		if code := run(args[1:], io.Discard, &stderr); code != 0 {
			t.Fatalf("compose help example exit %d: %s", code, stderr.String())
		}
		return
	}
	t.Fatal("help has no compose example")
}
