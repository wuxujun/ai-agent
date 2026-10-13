package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wuxujun/ai-agent/internal/store"
)

func runAdmission(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("assess", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	directory := flags.String("directory", "", "existing dedicated sandbox")
	task := flags.String("task", "", "original task id")
	tenant := flags.String("tenant", "", "explicit tenant")
	workspaceEvidence := flags.String("workspace-evidence", "", "operator-selected workspace baseline file")
	manifest := flags.String("manifest", "", "optional dependency manifest file")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *directory == "" || *task == "" || *tenant == "" {
		fmt.Fprintln(stderr, "invalid admission arguments")
		return 2
	}
	var data, workspaceData []byte
	var err error
	if *manifest != "" {
		data, err = readAdmissionInput(*manifest, store.MaxTaskRestoreManifestBytes)
		if err != nil {
			fmt.Fprintln(stderr, "cannot read dependency manifest within budget")
			return 1
		}
	}
	if *workspaceEvidence != "" {
		workspaceData, err = readAdmissionInput(*workspaceEvidence, store.MaxTaskRestoreWorkspaceEvidenceBytes)
		if err != nil {
			fmt.Fprintln(stderr, "cannot read workspace evidence within budget")
			return 1
		}
	}
	report, err := store.AssessPersistentTaskRestoreAdmissionWithWorkspace(ctx, *directory, *task, *tenant, data, workspaceData)
	if err != nil {
		fmt.Fprintln(stderr, "read-only restore admission assessment failed")
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		fmt.Fprintln(stderr, "cannot write admission report")
		return 1
	}
	// Diagnostic completion never grants admission. Distinguish a blocked result
	// from a read/parse failure so scripts cannot confuse success with permission.
	return 3
}

func readAdmissionInput(path string, limit int) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, errors.New("invalid admission input")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, errors.New("changed admission input")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if err != nil || statErr != nil || closeErr != nil || len(data) > limit || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("cannot read admission input")
	}
	return data, nil
}
