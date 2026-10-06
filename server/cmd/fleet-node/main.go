// fleet-node exposes only fixed runtime commands; it is not a general exec helper.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
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
