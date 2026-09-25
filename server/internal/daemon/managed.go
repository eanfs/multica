package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidManagedEnrollmentResponse marks an enrollment response that fails
// any carrier/execution identity check. Callers use it to distinguish a
// protocol mismatch from a transport failure.
var ErrInvalidManagedEnrollmentResponse = errors.New("invalid managed enrollment response")

// bootstrapManaged performs the one-time managed sandbox handshake: read the
// single-use mse_ secret, exchange it for the mdt_ credential and the enrolled
// runtime projection, install that identity into the daemon's existing
// indexes, and only then replace the client's long-lived token. It contacts
// exactly one endpoint; no workstation workspace discovery runs here.
func (d *Daemon) bootstrapManaged(ctx context.Context) error {
	if !d.cfg.Managed.Enabled {
		return errors.New("managed bootstrap called without managed mode")
	}
	token, err := readManagedEnrollmentToken(d.cfg.Managed.EnrollmentTokenFile)
	if err != nil {
		return err
	}
	resp, err := d.client.EnrollManaged(ctx, token)
	if err != nil {
		return fmt.Errorf("enroll managed sandbox: %w", err)
	}
	if err := d.installManagedEnrollment(resp); err != nil {
		return err
	}
	// Exactly once, and only after the response passed validation: the
	// enrollment secret is single-use, so it must never become the long-lived
	// credential.
	d.client.SetToken(resp.DaemonToken)
	d.cfg.DaemonID = resp.DaemonID
	if d.logger != nil {
		d.logger.Info("managed sandbox enrolled",
			"workspace_id", resp.WorkspaceID,
			"runtime_id", resp.Runtime.ID,
			"daemon_id", resp.DaemonID,
		)
	}
	return nil
}

// installManagedEnrollment validates the enrolled identity and installs
// exactly one workspaceState and one in-memory runtime. The persisted runtime
// projection stays aurora_managed/cloud; the installed runtime launches as the
// execution provider (claude) under the enrolled daemon identity.
func (d *Daemon) installManagedEnrollment(resp ManagedEnrollmentResponse) error {
	if resp.ExecutionProvider != "claude" || resp.MaxConcurrency != 1 {
		return fmt.Errorf("%w: execution_provider=%q max_concurrency=%d", ErrInvalidManagedEnrollmentResponse, resp.ExecutionProvider, resp.MaxConcurrency)
	}
	if strings.TrimSpace(resp.WorkspaceID) == "" || strings.TrimSpace(resp.DaemonID) == "" || strings.TrimSpace(resp.DaemonToken) == "" {
		return fmt.Errorf("%w: missing workspace, daemon, or token identity", ErrInvalidManagedEnrollmentResponse)
	}
	runtime := resp.Runtime
	if strings.TrimSpace(runtime.ID) == "" {
		return fmt.Errorf("%w: missing runtime id", ErrInvalidManagedEnrollmentResponse)
	}
	if runtime.Provider != "aurora_managed" || runtime.RuntimeMode != "cloud" {
		return fmt.Errorf("%w: persisted runtime provider=%q mode=%q", ErrInvalidManagedEnrollmentResponse, runtime.Provider, runtime.RuntimeMode)
	}
	if runtime.WorkspaceID != resp.WorkspaceID {
		return fmt.Errorf("%w: runtime workspace %q does not match enrolled workspace %q", ErrInvalidManagedEnrollmentResponse, runtime.WorkspaceID, resp.WorkspaceID)
	}
	runtime.Provider = resp.ExecutionProvider
	runtime.DaemonID = resp.DaemonID

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.workspaces) != 0 || len(d.runtimeIndex) != 0 {
		return fmt.Errorf("%w: daemon already has runtime state", ErrInvalidManagedEnrollmentResponse)
	}
	if d.workspaces == nil {
		d.workspaces = make(map[string]*workspaceState)
	}
	if d.runtimeIndex == nil {
		d.runtimeIndex = make(map[string]Runtime)
	}
	d.workspaces[resp.WorkspaceID] = newWorkspaceState(resp.WorkspaceID, []string{runtime.ID}, "", nil, nil)
	d.runtimeIndex[runtime.ID] = runtime
	return nil
}
