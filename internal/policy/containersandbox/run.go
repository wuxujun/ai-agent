package containersandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Run receives a workspace already authorized by the caller's workspace policy.
// It never pulls an image, selects a remote daemon, or falls back to host exec.
func Run(ctx context.Context, c Config, workspace, name string, args, extra []string, stdout, stderr io.Writer) (result error) {
	if err := c.Validate(); err != nil {
		return err
	}
	if !c.Docker() {
		return errors.New("sandbox Docker backend is not enabled")
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return errors.New("sandbox workspace is invalid")
	}
	if err := checkedWorkspace(ctx, workspace); err != nil {
		return err
	}
	if err := checkSocket(c.SocketPath); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) {
		return errors.New("sandbox execution requires a finite future deadline")
	}
	client := newClient(c.SocketPath)
	defer client.transport.CloseIdleConnections()
	if err := client.checkDaemon(ctx); err != nil {
		return err
	}
	var image imageInfo
	if _, err := client.control(ctx, http.MethodGet, "/images/"+url.PathEscape(c.Image)+"/json", nil, &image, http.StatusOK); err != nil {
		return err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return errors.New("sandbox execution identity could not be generated")
	}
	nonce := hex.EncodeToString(random)
	containerName := "ai-agent-task-" + nonce
	spec, err := buildSpec(c, image, workspace, name, args, extra, os.Environ(), nonce, time.Until(deadline))
	if err != nil {
		return err
	}
	var created struct {
		ID       string `json:"Id"`
		Warnings []string
	}
	// A canceled/lost create response may still leave a container. Cleanup uses
	// our unguessable name and verifies its ownership label before removing it.
	containerID := ""
	defer func() {
		if result != nil && ctx.Err() != nil {
			result = ctx.Err()
		}
		if cleanupErr := client.removeOwned(containerID, containerName, nonce); cleanupErr != nil {
			result = errors.Join(result, cleanupErr)
		}
	}()
	if _, err := client.control(ctx, http.MethodPost, "/containers/create?name="+containerName, spec, &created, http.StatusCreated); err != nil {
		return err
	}
	if !validContainerID(created.ID) || len(created.Warnings) != 0 {
		return errors.New("sandbox create response is invalid or reports unenforced settings")
	}
	containerID = created.ID
	inspected, _, err := client.inspect(ctx, containerID)
	if err != nil {
		return err
	}
	if err := verifyCreated(inspected, spec); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.attach(runCtx, containerID)
	if err != nil {
		return err
	}
	readDone := make(chan struct{})
	var readErr error
	go func() { defer close(readDone); readErr = copyOutput(stream.reader, stdout, stderr) }()
	defer func() { stream.Close(); cancel(); <-readDone }()
	if _, err := client.control(runCtx, http.MethodPost, "/containers/"+containerID+"/start", nil, nil, http.StatusNoContent); err != nil {
		return err
	}
	var waited struct {
		StatusCode *int
		Error      *struct{ Message string }
	}
	waitDone := make(chan struct{})
	var waitErr error
	go func() {
		defer close(waitDone)
		_, waitErr = client.call(runCtx, http.MethodPost, "/containers/"+containerID+"/wait?condition=not-running", nil, &waited, http.StatusOK)
	}()
	defer func() { cancel(); <-waitDone }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-readDone:
		if readErr != nil {
			return readErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waitDone:
		}
	case <-waitDone:
	}
	if waitErr != nil {
		return waitErr
	}
	if waited.StatusCode == nil || *waited.StatusCode < 0 || *waited.StatusCode > 255 || waited.Error != nil {
		return errors.New("sandbox wait response is invalid")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-readDone:
	}
	if readErr != nil {
		return readErr
	}
	exited, _, err := client.inspect(ctx, containerID)
	if err != nil {
		return err
	}
	if exited.State.Running || exited.State.Status != "exited" || exited.State.ExitCode != *waited.StatusCode || exited.State.Error != "" {
		return errors.New("sandbox exit state could not be confirmed")
	}
	if *waited.StatusCode != 0 {
		return &ExitError{Code: *waited.StatusCode, OOMKilled: exited.State.OOMKilled}
	}
	return nil
}

// Preflight is performed at server startup. Commands recheck the daemon and
// immutable image at the point of use; startup success is not cached forever.
func Preflight(ctx context.Context, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !c.Docker() {
		return errors.New("sandbox Docker backend is not enabled")
	}
	if err := checkSocket(c.SocketPath); err != nil {
		return err
	}
	client := newClient(c.SocketPath)
	defer client.transport.CloseIdleConnections()
	if err := client.checkDaemon(ctx); err != nil {
		return err
	}
	var image imageInfo
	if _, err := client.control(ctx, http.MethodGet, "/images/"+url.PathEscape(c.Image)+"/json", nil, &image, http.StatusOK); err != nil {
		return err
	}
	if image.ID != c.Image || image.Config.Labels[imageContractLabel] != "1" || len(image.Config.Volumes) != 0 {
		return errors.New("sandbox image does not satisfy the immutable execution image contract")
	}
	return nil
}
