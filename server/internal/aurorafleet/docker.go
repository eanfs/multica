package aurorafleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// fleetLabel marks containers the fleet owns so List can distinguish them from
// unrelated containers on the same host.
const fleetLabel = "multica-aurora-node=1"

// dockerPSFormat is the shared `docker ps` format used by Status and List. It
// emits one tab-separated row per container with the machine-readable State
// (rather than the localized {{.Status}} human text) so status mapping stays a
// fixed enum, plus the container's creation timestamp.
const dockerPSFormat = "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.State}}\t{{.CreatedAt}}"

// DockerBackend provisions nodes as Docker containers via the docker CLI. It
// shells out to `docker` rather than depending on the Docker SDK so the fleet
// controller stays a single self-contained binary with no new runtime
// dependencies. Container ids are the node ids.
type DockerBackend struct {
	dockerPath string
}

// NewDockerBackend returns a DockerBackend that talks to the `docker` binary.
// The node spec (image and environment) is owned by the Controller and passed
// through CreateRequest; the backend itself has no configuration.
func NewDockerBackend() *DockerBackend {
	return &DockerBackend{dockerPath: "docker"}
}

// run runs docker and returns its stdout, folding stderr into the error on
// failure. Most operations only care about stdout.
func (d *DockerBackend) run(ctx context.Context, args ...string) ([]byte, error) {
	stdout, _, err := d.runSplit(ctx, args...)
	return stdout, err
}

// runSplit runs docker capturing stdout and stderr separately, so Exec can
// report them faithfully instead of duplicating one combined buffer.
func (d *DockerBackend) runSplit(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, d.dockerPath, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if isDockerUnavailable(err) {
			return nil, nil, fmt.Errorf("%w: %s", ErrUnavailable, strings.TrimSpace(errBuf.String()))
		}
		return nil, nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errBuf.String()))
	}
	return outBuf.Bytes(), errBuf.Bytes(), nil
}

func (d *DockerBackend) Create(ctx context.Context, req CreateRequest) (Node, error) {
	if req.Image == "" {
		return Node{}, errors.New("no sandbox image configured")
	}

	args := []string{"run", "-d", "--label", fleetLabel}
	if req.Name != "" {
		args = append(args, "--name", req.Name)
	}
	args = appendMapFlags(args, "-e", req.Env)
	args = appendMapFlags(args, "--label", req.Labels)
	args = append(args, req.Image)

	out, err := d.run(ctx, args...)
	if err != nil {
		return Node{}, err
	}
	// docker run -d prints the new container id. The node is already running;
	// there is no need for a second `docker ps` to describe a container we just
	// created ourselves.
	id := strings.TrimSpace(string(out))
	created := now()
	return Node{
		ID:        id,
		Name:      req.Name,
		Image:     req.Image,
		Status:    StatusRunning,
		Labels:    maps.Clone(req.Labels),
		CreatedAt: created,
		UpdatedAt: created,
	}, nil
}

func (d *DockerBackend) Terminate(ctx context.Context, id string) error {
	_, err := d.run(ctx, "rm", "-f", id)
	return err
}

func (d *DockerBackend) Start(ctx context.Context, id string) error {
	_, err := d.run(ctx, "start", id)
	return err
}

func (d *DockerBackend) Stop(ctx context.Context, id string) error {
	_, err := d.run(ctx, "stop", id)
	return err
}

func (d *DockerBackend) Reboot(ctx context.Context, id string) error {
	_, err := d.run(ctx, "restart", id)
	return err
}

func (d *DockerBackend) Status(ctx context.Context, id string) (Node, error) {
	out, err := d.run(ctx, "ps", "-a", "--filter", "id="+id, "--format", dockerPSFormat)
	if err != nil {
		return Node{}, err
	}
	node, ok := parsePSLine(string(out))
	if !ok {
		return Node{}, ErrNotFound
	}
	return node, nil
}

func (d *DockerBackend) Exec(ctx context.Context, id string, command []string) (ExecResult, error) {
	if len(command) == 0 {
		return ExecResult{}, errors.New("exec command is empty")
	}
	args := append([]string{"exec", id}, command...)
	stdout, stderr, err := d.runSplit(ctx, args...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ExecResult{ExitCode: exitErr.ExitCode(), Stdout: stdout, Stderr: stderr}, nil
	}
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: 0, Stdout: stdout, Stderr: stderr}, nil
}

func (d *DockerBackend) List(ctx context.Context) ([]Node, error) {
	out, err := d.run(ctx, "ps", "-a", "--filter", "label="+fleetLabel, "--format", dockerPSFormat)
	if err != nil {
		return nil, err
	}
	var nodes []Node
	for _, line := range strings.Split(string(out), "\n") {
		if node, ok := parsePSLine(line); ok {
			nodes = append(nodes, node)
		}
	}
	return nodes, nil
}

func (d *DockerBackend) Ping(ctx context.Context) error {
	_, err := d.run(ctx, "info")
	return err
}

// parsePSLine decodes one docker ps row into a Node. It reports ok=false for a
// blank or malformed line so both Status (single row) and List (many rows) can
// share it.
func parsePSLine(line string) (Node, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Node{}, false
	}
	fields := strings.SplitN(line, "\t", 5)
	if len(fields) < 4 {
		return Node{}, false
	}
	return Node{
		ID:        strings.TrimSpace(fields[0]),
		Name:      strings.TrimSpace(fields[1]),
		Image:     strings.TrimSpace(fields[2]),
		Status:    dockerStateToNodeStatus(fields[3]),
		CreatedAt: parseDockerTime(fields[4]),
		UpdatedAt: now(),
	}, true
}

// dockerStateToNodeStatus maps a Docker container State (created/running/
// restarting/removing/exited/dead) onto the fleet's lifecycle vocabulary. State
// is the machine-readable field; it is deliberately not {{.Status}}, whose
// localized human text would force prefix matching.
func dockerStateToNodeStatus(state string) Status {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "created":
		return StatusProvisioning
	case "running":
		return StatusRunning
	case "restarting":
		return StatusRebooting
	case "removing":
		return StatusTerminating
	case "dead":
		return StatusError
	default: // "exited", "paused", unknown
		return StatusStopped
	}
}

// dockerTimeLayouts are the formats docker emits for a container timestamp,
// tried in order. Modern docker ps {{.CreatedAt}} is RFC3339; older versions
// use a space-separated variant with a trailing zone name.
var dockerTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05 -0700 MST",
}

func parseDockerTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range dockerTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func isDockerUnavailable(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		// The binary itself could not be found or launched.
		return true
	}
	// `docker` exits 125 when the daemon is unreachable.
	return exitErr.ExitCode() == 125
}

// appendMapFlags appends flag k=v for each entry of m in sorted-key order, so
// arg construction is deterministic regardless of map iteration order.
func appendMapFlags(args []string, flag string, m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, flag, k+"="+m[k])
	}
	return args
}
