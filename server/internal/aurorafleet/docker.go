package aurorafleet

import (
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

// DockerBackend provisions nodes as Docker containers via the docker CLI. It
// shells out to `docker` rather than depending on the Docker SDK so the fleet
// controller stays a single self-contained binary with no new runtime
// dependencies. Container ids are the node ids.
type DockerBackend struct {
	// Image is the default sandbox image used when a CreateRequest omits one.
	Image string
	// DefaultEnv is merged into every container's environment. The controller
	// populates it with the server URL and managed-registration secret.
	DefaultEnv map[string]string
	dockerPath string
}

// NewDockerBackend returns a DockerBackend that talks to the `docker` binary.
func NewDockerBackend(image string, defaultEnv map[string]string) *DockerBackend {
	return &DockerBackend{
		Image:      image,
		DefaultEnv: defaultEnv,
		dockerPath: "docker",
	}
}

func (d *DockerBackend) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.dockerPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isDockerUnavailable(err) {
			return nil, fmt.Errorf("%w: %s", ErrUnavailable, strings.TrimSpace(string(out)))
		}
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (d *DockerBackend) Create(ctx context.Context, req CreateRequest) (Node, error) {
	image := req.Image
	if image == "" {
		image = d.Image
	}
	if image == "" {
		return Node{}, errors.New("no sandbox image configured")
	}

	args := []string{"run", "-d", "--label", fleetLabel}
	if req.Name != "" {
		args = append(args, "--name", req.Name)
	}
	env := mergeEnv(d.DefaultEnv, req.Env)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, image)

	out, err := d.run(ctx, args...)
	if err != nil {
		return Node{}, err
	}
	id := strings.TrimSpace(string(out))
	return d.Status(ctx, id)
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
	// A single tab-separated row keeps parsing trivial and stable across Docker
	// versions.
	out, err := d.run(ctx, "ps", "-a", "--filter", "id="+id,
		"--format", "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.CreatedAt}}")
	if err != nil {
		return Node{}, err
	}
	row := firstNonEmptyLine(out)
	if row == "" {
		return Node{}, ErrNotFound
	}
	fields := strings.SplitN(row, "\t", 5)
	if len(fields) < 4 {
		return Node{}, fmt.Errorf("unexpected docker ps output: %q", row)
	}
	created, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(fields[4]))
	return Node{
		ID:        strings.TrimSpace(fields[0]),
		Name:      strings.TrimSpace(fields[1]),
		Image:     strings.TrimSpace(fields[2]),
		Status:    dockerStatusToNodeStatus(strings.TrimSpace(fields[3])),
		CreatedAt: created,
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (d *DockerBackend) Exec(ctx context.Context, id string, command []string) (ExecResult, error) {
	if len(command) == 0 {
		return ExecResult{}, errors.New("exec command is empty")
	}
	args := append([]string{"exec", id}, command...)
	out, err := d.run(ctx, args...)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ExecResult{ExitCode: exitErr.ExitCode(), Stdout: out, Stderr: out}, nil
	}
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: 0, Stdout: out}, nil
}

func (d *DockerBackend) List(ctx context.Context) ([]Node, error) {
	out, err := d.run(ctx, "ps", "-a", "--filter", "label="+fleetLabel,
		"--format", "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.CreatedAt}}")
	if err != nil {
		return nil, err
	}
	var nodes []Node
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 5)
		if len(fields) < 4 {
			continue
		}
		created, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(fields[4]))
		nodes = append(nodes, Node{
			ID:        strings.TrimSpace(fields[0]),
			Name:      strings.TrimSpace(fields[1]),
			Image:     strings.TrimSpace(fields[2]),
			Status:    dockerStatusToNodeStatus(strings.TrimSpace(fields[3])),
			CreatedAt: created,
			UpdatedAt: time.Now().UTC(),
		})
	}
	return nodes, nil
}

func (d *DockerBackend) Ping(ctx context.Context) error {
	_, err := d.run(ctx, "info")
	return err
}

// dockerStatusToNodeStatus maps a Docker container state prefix ("Up 5 minutes",
// "Exited (1) 2 hours ago") onto the fleet's lifecycle vocabulary.
func dockerStatusToNodeStatus(s string) Status {
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "up"):
		return StatusRunning
	case strings.HasPrefix(lower, "restarting"):
		return StatusRebooting
	case strings.HasPrefix(lower, "created"):
		return StatusProvisioning
	case strings.HasPrefix(lower, "exited"):
		return StatusStopped
	case strings.HasPrefix(lower, "dead"):
		return StatusError
	default:
		return StatusStopped
	}
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

func mergeEnv(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

func firstNonEmptyLine(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}
