// task-restore performs offline isolated recovery drills. It has no runtime
// configuration, online Store connection, executor, indexer or provider client.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/wuxujun/ai-agent/internal/store"
)

type restoreOutput struct {
	Directory    string                   `json:"sandbox_directory,omitempty"`
	Verification *store.TaskRestoreReport `json:"verification,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "assess" {
		return runAdmission(ctx, args[1:], stdout, stderr)
	}
	if len(args) == 0 || (args[0] != "import" && args[0] != "verify") {
		fmt.Fprintln(stderr, "usage: task-restore import --file FILE --tenant TENANT --parent DIRECTORY [--byte-budget N] | verify --directory DIRECTORY --task TASK --tenant TENANT | assess --directory DIRECTORY --task TASK --tenant TENANT [--manifest FILE] [--workspace-evidence FILE]")
		return 2
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tenant := flags.String("tenant", "", "explicit tenant")
	var input, parent, directory, task string
	budget := 1 << 20
	if args[0] == "import" {
		flags.StringVar(&input, "file", "", "joint export file")
		flags.StringVar(&parent, "parent", "", "existing sandbox parent")
		flags.IntVar(&budget, "byte-budget", budget, "maximum artifact bytes")
	} else {
		flags.StringVar(&directory, "directory", "", "dedicated sandbox directory")
		flags.StringVar(&task, "task", "", "original task id")
	}
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || *tenant == "" || (args[0] == "import" && (input == "" || parent == "" || budget < 4096 || budget > store.MaxTaskExportBytes)) || (args[0] == "verify" && (directory == "" || task == "")) {
		fmt.Fprintln(stderr, "invalid restore arguments")
		return 2
	}
	var report *store.TaskRestoreReport
	var err error
	if args[0] == "import" {
		info, statErr := os.Stat(input)
		if statErr != nil || !info.Mode().IsRegular() {
			fmt.Fprintln(stderr, "joint export must be a regular file")
			return 1
		}
		file, openErr := os.Open(input)
		if openErr != nil {
			fmt.Fprintln(stderr, "cannot read joint export file")
			return 1
		}
		data, readErr := io.ReadAll(io.LimitReader(file, int64(budget)+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			fmt.Fprintln(stderr, "cannot read joint export file")
			return 1
		}
		report, directory, err = store.VerifyPersistentTaskRestore(ctx, data, *tenant, parent, budget)
	} else {
		sandbox, openErr := store.OpenPersistentTaskRestoreSandbox(ctx, directory)
		if openErr != nil {
			fmt.Fprintln(stderr, "cannot open dedicated restore sandbox")
			return 1
		}
		report, err = sandbox.Verify(ctx, task, *tenant)
		closeErr := sandbox.Close()
		if err == nil {
			err = closeErr
		}
	}
	if directory != "" {
		if encodeErr := json.NewEncoder(stdout).Encode(restoreOutput{Directory: directory, Verification: report}); encodeErr != nil {
			fmt.Fprintln(stderr, "cannot write restore report")
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "isolated restore verification failed; any created sandbox is preserved")
		return 1
	}
	return 0
}
