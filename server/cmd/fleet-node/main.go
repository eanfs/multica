// fleet-node exposes only fixed runtime commands; it is not a general exec helper.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/fleet/model"
)

const runtimePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
const daemonExecutable = "/usr/local/bin/multica"

// exec is an internal seam: default tests never launch a daemon or resolve an agent.
func runNode(home, secrets, max string, exec func(string, []string, []string) error) error {
	count, err := strconv.Atoi(max)
	if err != nil || count <= 0 || strconv.Itoa(count) != max || exec == nil {
		return model.ErrInvalidRequest
	}
	// The managed Aurora profile supersedes the Claude-only CLI bootstrap: the
	// daemon exchanges the single-use enrollment secret for its own identity and
	// never reads a node token, API key or model URL from this container.
	if path := os.Getenv(model.AuroraEnrollmentFileEnv); path != "" {
		return runManagedNode(secrets, path, exec)
	}
	_, b, err := bootstrapFiles(home, secrets)
	if err != nil {
		return err
	}
	argv := []string{daemonExecutable, "daemon", "start", "--foreground", "--no-auto-update", "--no-auto-reload", "--max-concurrent-tasks", max, "--daemon-id", b.DaemonID, "--workspaces-root", model.WorkspacesRoot}
	env := []string{"PATH=" + runtimePath, "HOME=" + home, "ANTHROPIC_API_KEY=" + b.APIKey}
	if b.BaseURL != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+b.BaseURL)
	}
	if b.Model != "" {
		env = append(env, "ANTHROPIC_MODEL="+b.Model)
	}
	return exec(daemonExecutable, argv, env)
}

// runManagedNode validates the fixed enrollment secret and starts the managed
// daemon. The secret path is fixed by the Fleet container contract, so a caller
// cannot redirect it; the daemon inherits the container's exact environment.
func runManagedNode(secrets, path string, exec func(string, []string, []string) error) error {
	// The managed profile fixes both the secrets mount and the file name, so a
	// caller cannot redirect the one-time secret.
	if secrets == "" || path != filepath.Join(secrets, managedEnrollmentName) {
		return model.ErrInvalidRequest
	}
	if _, err := readManagedEnrollment(path); err != nil {
		return err
	}
	argv := []string{daemonExecutable, "daemon", "start", "--managed", "--foreground", "--managed-enrollment-token-file=" + path}
	return exec(daemonExecutable, argv, os.Environ())
}

// managedEnrollmentName is the fixed installer file name for the managed
// enrollment secret inside the read-only node secrets volume.
const managedEnrollmentName = "aurora-enrollment"

// readManagedEnrollment accepts only an owner-only regular file carrying one
// well-formed enrollment secret. Anything else fails closed.
func readManagedEnrollment(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 256 {
		return "", model.ErrInvalidRequest
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", model.ErrInvalidRequest
	}
	token := strings.TrimSpace(string(raw))
	if !model.ValidEnrollmentToken(token) {
		return "", model.ErrInvalidRequest
	}
	return token, nil
}

func command(args []string, out io.Writer) error {
	if len(args) != 1 {
		return model.ErrInvalidRequest
	}
	switch args[0] {
	case "bootstrap":
		_, err := BootstrapFiles(model.NodeHome, "/secrets")
		return err
	case "run":
		return runNode(model.NodeHome, "/secrets", os.Getenv("FLEET_NODE_MAX_RUNS"), syscall.Exec)
	case "health":
		layout, _, err := readLayout(model.DataMount)
		if err != nil {
			return model.ErrUnknownHealth
		}
		observation, err := ReadObservation(&http.Client{Timeout: 5 * time.Second}, "http://127.0.0.1:19514/health")
		if err != nil || observation.DaemonID != layout.DaemonID {
			return model.ErrUnknownHealth
		}
		if err := json.NewEncoder(out).Encode(observationWire(observation)); err != nil {
			return model.ErrUnknownHealth
		}
		if !observation.Ready {
			return model.ErrUnknownHealth
		}
		return nil
	case "report-stats":
		wire, err := offlineReportStatsAt(model.DataMount)
		if err != nil {
			return model.ErrUnknownHealth
		}
		if json.NewEncoder(out).Encode(wire) != nil {
			return model.ErrUnknownHealth
		}
		return nil
	default:
		return model.ErrInvalidRequest
	}
}
func main() {
	if err := command(os.Args[1:], os.Stdout); err != nil {
		// Do not expose source errors, paths, profile bytes or syscall diagnostics.
		fmt.Fprintln(os.Stderr, "fleet-node command failed")
		os.Exit(1)
	}
}
