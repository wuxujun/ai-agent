package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTeamsDeploymentPreservesExternalChanges(t *testing.T) {
	for _, stage := range []string{"before deploy", "before restore"} {
		t.Run(stage, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "teams.yaml")
			if err := os.WriteFile(dst, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			d, err := lockTeamsDeployment(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer d.close()
			if _, err := lockTeamsDeployment(dst); err == nil {
				t.Fatal("second writer acquired lock")
			}
			if stage == "before restore" {
				if err := d.replace([]byte("candidate")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(dst, []byte("external edit"), 0600); err != nil {
				t.Fatal(err)
			}
			var gotErr error
			if stage == "before deploy" {
				gotErr = d.replace([]byte("candidate"))
			} else {
				gotErr = d.restore("http://127.0.0.1:1", "", time.Second)
			}
			if gotErr == nil {
				t.Fatal("must detect external edit")
			}
			data, _ := os.ReadFile(dst)
			if string(data) != "external edit" {
				t.Fatal("external edit overwritten")
			}
			backup, _ := os.ReadFile(d.backup)
			if string(backup) != "original" {
				t.Fatal("original backup missing")
			}
		})
	}
}

func TestTeamsDeploymentRestoresContentsAndPermissions(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "teams.yaml")
	if err := os.WriteFile(dst, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	d, err := lockTeamsDeployment(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.replace([]byte("candidate")); err != nil {
		t.Fatal(err)
	}
	if err := d.restore(srv.URL, "", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dst)
	info, _ := os.Stat(dst)
	if string(data) != "original" || info.Mode().Perm() != 0600 {
		t.Fatal("restore changed contents or permissions")
	}
	if _, err := os.Stat(d.backup); !os.IsNotExist(err) {
		t.Fatal("successful restore left backup")
	}
}

func TestReloadTimeoutAndRedirect(t *testing.T) {
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hanging.Close()
	start := time.Now()
	if err := reloadServiceTimeout(hanging.URL, "", 30*time.Millisecond); err == nil || !errors.Is(err, os.ErrDeadlineExceeded) && !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not bounded")
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed with credentials") }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	if err := reloadService(redirect.URL, "synthetic"); err == nil {
		t.Fatal("redirect must fail")
	}
}

func TestRunFailsWhenRestoreReloadFails(t *testing.T) {
	teams, skills := testRepoPaths(t)
	serviceTeams := filepath.Join(t.TempDir(), "teams.yaml")
	original, err := os.ReadFile(teams)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serviceTeams, original, 0600); err != nil {
		t.Fatal(err)
	}
	good := refinementService(t)
	reloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/config/reload" {
			reloads++
			if reloads == 2 {
				w.WriteHeader(500)
				return
			}
		}
		good.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	dataset := filepath.Join(t.TempDir(), "cases.yaml")
	if err := os.WriteFile(dataset, []byte(probeTestDataset), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := runAll([]string{"--teams-yaml", teams, "--skills-root", skills, "--team", "software", "--k", "1", "--service-url", srv.URL, "--service-teams-yaml", serviceTeams, "--input", dataset, "--out", t.TempDir()}, io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "restore service teams") {
		t.Fatalf("restore failure reported success: code=%d %s", code, &stderr)
	}
	got, _ := os.ReadFile(serviceTeams)
	if !bytes.Equal(got, original) {
		t.Fatal("original file not restored")
	}
	if _, err := os.Stat(serviceTeams + ".harness-refine.original"); err != nil {
		t.Fatal("failed restore must retain backup")
	}
}

// Opt-in real HTTP against the checked-in Postman simulation member.
func TestHarnessReloadHTTPSimulation(t *testing.T) {
	base := os.Getenv("AI_AGENT_TEST_HARNESS_RELOAD_URL")
	if base == "" {
		t.Skip("set AI_AGENT_TEST_HARNESS_RELOAD_URL for the local HTTP simulation")
	}
	for _, tc := range []struct {
		path    string
		wantErr bool
	}{{"/ok", false}, {"/failure", true}, {"/hang", true}} {
		t.Run(tc.path, func(t *testing.T) {
			err := reloadServiceTimeout(base+tc.path, "synthetic", 100*time.Millisecond)
			if (err != nil) != tc.wantErr {
				t.Fatalf("unexpected reload result: %v", err)
			}
		})
	}
}
