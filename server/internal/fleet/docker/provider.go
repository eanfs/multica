package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	bytesPerMB      = 1024 * 1024
	nanoCPUsPerCPU  = 1e9
	maxHealthOutput = 1 << 20
)

// SDK is the Docker Engine API implementation of Provider. It never shells out
// to the Docker CLI.
type SDK struct {
	cli       *client.Client
	user      string
	healthCmd []string
}

// NewSDK connects to the Docker Engine at host (for example
// unix:///var/run/docker.sock). The host is explicit so Fleet cannot fall back
// to an unintended daemon.
func NewSDK(host string) (*SDK, error) {
	if strings.TrimSpace(host) == "" {
		return nil, errors.New("docker provider: host is required")
	}
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &SDK{
		cli:  cli,
		user: nodeUser,
		// The health command is constant: no caller input reaches exec. The
		// daemon listens on container loopback only.
		healthCmd: []string{"curl", "-fsS", healthURL},
	}, nil
}

// Close releases the client's HTTP transport.
func (s *SDK) Close() error {
	if s.cli == nil {
		return nil
	}
	return s.cli.Close()
}

func (s *SDK) Ping(ctx context.Context) error {
	if _, err := s.cli.Ping(ctx); err != nil {
		return fmt.Errorf("ping docker engine: %w", err)
	}
	return nil
}

func (s *SDK) InspectImage(ctx context.Context, image string) error {
	if image == "" {
		return errors.New("inspect image: image is required")
	}
	if _, err := s.cli.ImageInspect(ctx, image); err != nil {
		return fmt.Errorf("inspect image %q: %w", image, err)
	}
	return nil
}

func (s *SDK) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	if name == "" {
		return errors.New("ensure network: name is required")
	}
	_, err := s.cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge", Labels: labels})
	if err == nil {
		return nil
	}
	// The namespace network outlives individual nodes, so an existing network
	// satisfies the request. Verify it instead of matching on error text.
	existing, inspectErr := s.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if inspectErr != nil {
		return fmt.Errorf("create network %q: %w", name, err)
	}
	if existing.Driver != "bridge" {
		return fmt.Errorf("network %q exists with driver %q, want bridge", name, existing.Driver)
	}
	return nil
}

func (s *SDK) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	if name == "" {
		return errors.New("ensure volume: name is required")
	}
	// VolumeCreate is idempotent: an existing volume keeps its current labels.
	if _, err := s.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels}); err != nil {
		return fmt.Errorf("create volume %q: %w", name, err)
	}
	return nil
}

func (s *SDK) CreateContainer(ctx context.Context, spec NodeSpec) (string, error) {
	if err := validateNodeSpec(spec); err != nil {
		return "", err
	}
	pidsLimit := spec.PidsLimit
	config := &container.Config{
		Image:  spec.Image,
		Env:    spec.Env,
		User:   s.user,
		Labels: spec.Labels,
		Healthcheck: &container.HealthConfig{
			Test:        append([]string{"CMD"}, s.healthCmd...),
			Interval:    10 * time.Second,
			Timeout:     5 * time.Second,
			Retries:     3,
			StartPeriod: 2 * time.Minute,
		},
	}
	hostConfig := &container.HostConfig{
		// Nodes never restart themselves; an explicit user start is the only
		// way back from a stop.
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		NetworkMode:   container.NetworkMode(spec.Network),
		Resources: container.Resources{
			Memory:    spec.MemoryMB * bytesPerMB,
			NanoCPUs:  int64(math.Round(spec.CPU * nanoCPUsPerCPU)),
			PidsLimit: &pidsLimit,
		},
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: spec.DataVolume, Target: dataMountPath},
			{Type: mount.TypeVolume, Source: spec.SecretsVolume, Target: secretsMountPath, ReadOnly: true},
		},
		// No privileged mode, host PID/IPC/network namespace, or published
		// ports: the zero values are the confinement.
	}
	resp, err := s.cli.ContainerCreate(ctx, config, hostConfig, nil, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("create container %q: %w", spec.Name, err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("create container %q: docker returned no id", spec.Name)
	}
	return resp.ID, nil
}

func (s *SDK) WriteSecrets(ctx context.Context, containerID string, files map[string][]byte) error {
	if containerID == "" {
		return errors.New("write secrets: container id is required")
	}
	if len(files) == 0 {
		return errors.New("write secrets: no files")
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			return fmt.Errorf("write secrets: invalid file name %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	// Copy the files while the container is created but not started; the tar
	// headers make them readable by the node UID and nobody else.
	dir := strings.TrimPrefix(secretsMountPath, "/") + "/"
	now := time.Now()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     dir,
		Mode:     0o750,
		Uid:      nodeUID,
		Gid:      nodeGID,
		ModTime:  now,
	}); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     dir + name,
			Mode:     0o600,
			Uid:      nodeUID,
			Gid:      nodeGID,
			Size:     int64(len(body)),
			ModTime:  now,
		}); err != nil {
			return fmt.Errorf("write secrets: %w", err)
		}
		if _, err := tw.Write(body); err != nil {
			return fmt.Errorf("write secrets: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	if err := s.cli.CopyToContainer(ctx, containerID, "/", &buf, container.CopyToContainerOptions{CopyUIDGID: true}); err != nil {
		return fmt.Errorf("write secrets to container %q: %w", containerID, err)
	}
	return nil
}

func (s *SDK) Start(ctx context.Context, containerID string) error {
	if err := s.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start container %q: %w", containerID, err)
	}
	return nil
}

func (s *SDK) Stop(ctx context.Context, containerID string, timeout time.Duration) error {
	if err := s.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: stopTimeout(timeout)}); err != nil {
		return fmt.Errorf("stop container %q: %w", containerID, err)
	}
	return nil
}

func (s *SDK) Restart(ctx context.Context, containerID string, timeout time.Duration) error {
	if err := s.cli.ContainerRestart(ctx, containerID, container.StopOptions{Timeout: stopTimeout(timeout)}); err != nil {
		return fmt.Errorf("restart container %q: %w", containerID, err)
	}
	return nil
}

func (s *SDK) Remove(ctx context.Context, containerID string, removeVolumes bool) error {
	if err := s.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true, RemoveVolumes: removeVolumes}); err != nil {
		// Deletion is retried after an uncertain outcome; a container that is
		// already gone satisfies the request.
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove container %q: %w", containerID, err)
	}
	return nil
}

func (s *SDK) RemoveVolume(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("remove volume: name is required")
	}
	if err := s.cli.VolumeRemove(ctx, name, true); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove volume %q: %w", name, err)
	}
	return nil
}

func (s *SDK) Inspect(ctx context.Context, containerID string) (ObservedContainer, error) {
	info, err := s.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return ObservedContainer{}, fmt.Errorf("inspect container %q: %w", containerID, err)
	}
	observed := ObservedContainer{ID: info.ID}
	if info.State == nil {
		return observed, nil
	}
	observed.State = string(info.State.Status)
	observed.Healthy = info.State.Health != nil && info.State.Health.Status == container.Healthy
	return observed, nil
}

func (s *SDK) ListByLabels(ctx context.Context, labels map[string]string) ([]string, error) {
	if len(labels) == 0 {
		return nil, errors.New("list containers: at least one label is required")
	}
	args := filters.NewArgs()
	for key, value := range labels {
		args.Add("label", key+"="+value)
	}
	list, err := s.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	ids := make([]string, 0, len(list))
	for _, item := range list {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *SDK) ExecHealth(ctx context.Context, containerID string) ([]byte, error) {
	created, err := s.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          s.healthCmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create health exec in %q: %w", containerID, err)
	}
	attached, err := s.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("attach health exec in %q: %w", containerID, err)
	}
	defer attached.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, io.LimitReader(attached.Reader, maxHealthOutput)); err != nil {
		return nil, fmt.Errorf("read health exec in %q: %w", containerID, err)
	}
	inspected, err := s.cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("inspect health exec in %q: %w", containerID, err)
	}
	if inspected.ExitCode != 0 {
		return stdout.Bytes(), fmt.Errorf("health command in %q exited with code %d: %s", containerID, inspected.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// stopTimeout converts a graceful stop budget into Docker's seconds field.
// A non-positive budget defers to the daemon default instead of killing
// immediately.
func stopTimeout(timeout time.Duration) *int {
	if timeout <= 0 {
		return nil
	}
	seconds := int(math.Ceil(timeout.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return &seconds
}

var _ Provider = (*SDK)(nil)
