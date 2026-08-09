package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"
)

type appHTTPServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

type appTaskManager interface {
	Shutdown(context.Context) error
}

type appRuntime struct {
	server          appHTTPServer
	tasks           appTaskManager
	bus             *approvalBusRuntime
	reload          func() error
	shutdownTimeout time.Duration
}

// runApp owns the process-level serving lifecycle. Startup wiring remains in
// run, while signal handling and ordered shutdown are isolated here so they can
// be verified without opening a real listener or sending process signals.
func runApp(runtime appRuntime, signals <-chan os.Signal) error {
	if runtime.shutdownTimeout <= 0 {
		runtime.shutdownTimeout = 10 * time.Second
	}

	serveResult := make(chan error, 1)
	go func() {
		err := runtime.server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveResult <- err
	}()

	var runErr error
	serverStopped := false

waitLoop:
	for {
		select {
		case err := <-serveResult:
			serverStopped = true
			if err != nil {
				runErr = fmt.Errorf("HTTP server failed: %w", err)
			}
			break waitLoop
		case sig, ok := <-signals:
			if !ok {
				runErr = errors.New("server signal channel closed")
				break waitLoop
			}
			if sig == syscall.SIGHUP {
				slog.Info("SIGHUP received, reloading configuration")
				if runtime.reload != nil {
					if err := runtime.reload(); err != nil {
						slog.Error("config reload failed", "error", err)
					}
				}
				continue
			}
			slog.Info("shutdown signal received, draining connections")
			break waitLoop
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), runtime.shutdownTimeout)
	defer shutdownCancel()
	if err := runtime.server.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("HTTP shutdown: %w", err))
	}
	if !serverStopped {
		select {
		case err := <-serveResult:
			if err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("HTTP server failed: %w", err))
			}
		case <-shutdownCtx.Done():
			runErr = errors.Join(runErr, fmt.Errorf("waiting for HTTP server shutdown: %w", shutdownCtx.Err()))
		}
	}

	slog.Info("waiting for background tasks to finish")
	taskDrainCtx, taskDrainCancel := context.WithTimeout(context.Background(), runtime.shutdownTimeout)
	defer taskDrainCancel()
	if err := runtime.tasks.Shutdown(taskDrainCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("background task shutdown: %w", err))
	}
	if err := runtime.bus.Close(); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("approval bus shutdown: %w", err))
	}
	slog.Info("shutdown complete")
	return runErr
}
