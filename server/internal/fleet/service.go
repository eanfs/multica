package fleet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/fleet/docker"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Service errors are mapped to HTTP statuses by writeServiceError. Callers must
// wrap, not replace, them so errors.Is keeps working.
var (
	ErrSpecNotAllowed    = errors.New("spec not allowed")
	ErrNodeBusy          = errors.New("node busy")
	ErrConflict          = errors.New("operation conflict")
	ErrNotFound          = errors.New("node not found")
	ErrInvalidRequest    = errors.New("invalid request")
	ErrDockerUnavailable = errors.New("docker unavailable")
	ErrImageMissing      = errors.New("runtime image missing")
	ErrStateUncertain    = errors.New("node state uncertain")
	ErrNotConfigured     = errors.New("fleet not configured")
)

const (
	// failureWriteTimeout bounds the bookkeeping done after a create failure so
	// a canceled request still records the failed node without hanging.
	failureWriteTimeout = 5 * time.Second
	// launchCleanupTimeout bounds Docker cleanup when a launch fails; the
	// caller's context may already be canceled.
	launchCleanupTimeout = 30 * time.Second
	// operationStopTimeout is the graceful budget for stop and restart.
	operationStopTimeout = 30 * time.Second
	// healthFreshWindow matches the staleness window used for claim barriers:
	// a health sample older than this cannot make a node look executable.
	healthFreshWindow = 30 * time.Second
	// defaultListLimit and maxListLimit bound one owner's node listing.
	defaultListLimit = 20
	maxListLimit     = 100
	// nodeContainerPrefix identifies nodes created by this checkout.
	nodeContainerPrefix = "multica-fleet-"
)

// Service owns the Fleet node lifecycle: it validates create intents, talks to
// Docker through Provider, and persists node state in the shared PostgreSQL.
type Service struct {
	cfg      Config
	store    *store.Store
	provider docker.Provider
	creds    *Credentials
	secret   string
	now      func() time.Time
}

// NewService assembles a Service. The caller owns the Store/Provider lifetimes.
func NewService(cfg Config, st *store.Store, provider docker.Provider, creds *Credentials, secret string) *Service {
	return &Service{cfg: cfg, store: st, provider: provider, creds: creds, secret: secret, now: time.Now}
}

// CreateNodeRequest is the only create intent a client may send. There is no
// field for an image, region, mount, script, or credentials.
type CreateNodeRequest struct {
	Name           string `json:"name"`
	SpecID         string `json:"spec_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

// PATIdentity mirrors the Cloud node-token verification response.
type PATIdentity struct {
	Valid            bool
	OwnerID          string
	InstanceID       string
	InstanceRecordID string
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Service) specByID(id string) (Spec, bool) {
	for _, spec := range s.cfg.Specs {
		if spec.ID == id {
			return spec, true
		}
	}
	return Spec{}, false
}

// CreateNode reserves a node for the owner, seeds its volumes and secrets, then
// starts the container. Any failure after reservation marks the node failed and
// revokes the credential it minted.
func (s *Service) CreateNode(ctx context.Context, ownerID string, req CreateNodeRequest) (db.FleetNode, error) {
	if s == nil {
		return db.FleetNode{}, ErrNotConfigured
	}
	spec, ok := s.specByID(req.SpecID)
	if !ok {
		return db.FleetNode{}, ErrSpecNotAllowed
	}
	if s.creds == nil {
		return db.FleetNode{}, ErrProfileMissing
	}
	profile, err := s.creds.Resolve(ownerID)
	if err != nil {
		return db.FleetNode{}, err
	}
	if s.store == nil || s.provider == nil {
		return db.FleetNode{}, ErrNotConfigured
	}
	if err := s.provider.Ping(ctx); err != nil {
		return db.FleetNode{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	// Fleet never pulls or builds as part of a create request: the approved
	// image must already exist locally.
	if err := s.provider.InspectImage(ctx, s.cfg.RuntimeImage); err != nil {
		return db.FleetNode{}, fmt.Errorf("%w: %v", ErrImageMissing, err)
	}
	ownerUUID, err := util.ParseUUID(ownerID)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("%w: owner id is not a UUID", ErrInvalidRequest)
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("encode node spec: %w", err)
	}
	node, err := s.store.ReserveNode(ctx, db.CreateFleetNodeParams{
		OwnerID:        ownerUUID,
		Namespace:      s.cfg.Namespace,
		Name:           req.Name,
		DaemonID:       uuid.NewString(),
		Image:          s.cfg.RuntimeImage,
		Spec:           specJSON,
		TargetState:    StateLaunching,
		IdempotencyKey: req.IdempotencyKey,
	}, s.cfg.MaxNodesPerOwner)
	if err != nil {
		return db.FleetNode{}, err
	}

	plain, hash, err := mintNodeToken()
	if err != nil {
		s.failNode(ctx, node, err)
		return db.FleetNode{}, err
	}
	if _, err := s.store.Queries().CreateFleetNodeCredential(ctx, db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: hash,
	}); err != nil {
		s.failNode(ctx, node, err)
		return db.FleetNode{}, fmt.Errorf("create node credential: %w", err)
	}
	if err := s.launchNode(ctx, node, profile, plain); err != nil {
		s.failNode(ctx, node, err)
		return db.FleetNode{}, err
	}
	created, err := s.reloadNode(ctx, node.ID)
	if err != nil {
		return db.FleetNode{}, err
	}
	return created, nil
}

// ListNodes returns only the caller's nodes, newest first. The namespace is
// always applied by the service, never supplied by the client.
func (s *Service) ListNodes(ctx context.Context, ownerID string, limit, offset int) ([]db.FleetNode, error) {
	if s == nil || s.store == nil {
		return nil, ErrNotConfigured
	}
	ownerUUID, err := util.ParseUUID(ownerID)
	if err != nil {
		return nil, fmt.Errorf("%w: owner id is not a UUID", ErrInvalidRequest)
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	return s.store.Queries().ListFleetNodesByOwner(ctx, db.ListFleetNodesByOwnerParams{
		OwnerID:     ownerUUID,
		Namespace:   s.cfg.Namespace,
		LimitCount:  int32(limit),
		OffsetCount: clampOffset(offset),
	})
}

// NodeRef selects one node by Docker container id (the client-facing instance
// id) or by fleet node id. A node whose launch failed has no container, so
// delete/status/diagnose must be able to address it by node id. Exactly one
// field may be set.
type NodeRef struct {
	InstanceID string
	NodeID     string
}

// Operate applies start/stop/reboot/status/delete to one owner's node. The
// instance id is the Docker container id; any owner or namespace mismatch reads
// as not found. The direct implementation applies the operation immediately;
// idempotencyKey is carried for the journaled operation path.
func (s *Service) Operate(ctx context.Context, ownerID, instanceID, operation, idempotencyKey string) (db.FleetNode, error) {
	return s.OperateRef(ctx, ownerID, NodeRef{InstanceID: instanceID}, operation, idempotencyKey)
}

// OperateRef is Operate with a node reference, so a failed launch that never
// produced a container can still be deleted by its fleet node id.
func (s *Service) OperateRef(ctx context.Context, ownerID string, ref NodeRef, operation, idempotencyKey string) (db.FleetNode, error) {
	if s == nil || s.store == nil || s.provider == nil {
		return db.FleetNode{}, ErrNotConfigured
	}
	node, err := s.findNode(ctx, ownerID, ref)
	if err != nil {
		return db.FleetNode{}, err
	}
	return s.applyOperation(ctx, node, operation)
}

// Diagnose reports the node daemon's own health. A node without a container or
// without a readable health payload is uncertain, never idle.
func (s *Service) Diagnose(ctx context.Context, node db.FleetNode) (docker.HealthReport, error) {
	if s == nil || s.provider == nil {
		return docker.HealthReport{}, ErrNotConfigured
	}
	if node.ContainerID == "" {
		// The node exists but never started a container. That is an unknown
		// daemon state, not a missing node.
		return docker.HealthReport{}, fmt.Errorf("%w: node has no container", ErrStateUncertain)
	}
	body, err := s.provider.ExecHealth(ctx, node.ContainerID)
	if err != nil {
		return docker.HealthReport{}, fmt.Errorf("%w: read daemon health: %v", ErrStateUncertain, err)
	}
	report, err := docker.ParseHealthReport(body)
	if err != nil {
		return docker.HealthReport{}, fmt.Errorf("%w: %v", ErrStateUncertain, err)
	}
	return report, nil
}

// VerifyPAT resolves an mcn_ node token to its owner and node identity. An
// unknown or revoked token is a normal invalid result, not an error.
func (s *Service) VerifyPAT(ctx context.Context, token string) (PATIdentity, bool, error) {
	if s == nil || s.store == nil {
		return PATIdentity{}, false, ErrNotConfigured
	}
	if !strings.HasPrefix(token, auth.CloudPATPrefix) {
		return PATIdentity{Valid: false}, false, nil
	}
	row, err := s.store.Queries().GetFleetNodeCredentialByHash(ctx, auth.HashToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PATIdentity{Valid: false}, false, nil
		}
		return PATIdentity{}, false, fmt.Errorf("verify node credential: %w", err)
	}
	// A token minted for another namespace's Fleet must not validate here,
	// even though the hash lookup itself is namespace-agnostic.
	if row.Namespace != s.cfg.Namespace {
		return PATIdentity{Valid: false}, false, nil
	}
	return PATIdentity{
		Valid:            true,
		OwnerID:          store.UUIDText(row.OwnerID),
		InstanceID:       row.ContainerID,
		InstanceRecordID: store.UUIDText(row.NodeID),
	}, true, nil
}

// findNode loads one node by container id or node id, always scoped to the
// service namespace and the caller's ownership. A foreign node and a missing
// node are indistinguishable to the caller.
func (s *Service) findNode(ctx context.Context, ownerID string, ref NodeRef) (db.FleetNode, error) {
	if s == nil || s.store == nil {
		return db.FleetNode{}, ErrNotConfigured
	}
	instanceID := strings.TrimSpace(ref.InstanceID)
	nodeID := strings.TrimSpace(ref.NodeID)
	if instanceID != "" && nodeID != "" {
		return db.FleetNode{}, fmt.Errorf("%w: send either instance_id or node_id, not both", ErrInvalidRequest)
	}
	if nodeID != "" {
		nodeUUID, err := util.ParseUUID(nodeID)
		if err != nil {
			return db.FleetNode{}, fmt.Errorf("%w: node id is not a UUID", ErrInvalidRequest)
		}
		ownerUUID, err := util.ParseUUID(ownerID)
		if err != nil {
			return db.FleetNode{}, fmt.Errorf("%w: owner id is not a UUID", ErrInvalidRequest)
		}
		node, err := s.store.Queries().GetFleetNodeByIDAndOwner(ctx, db.GetFleetNodeByIDAndOwnerParams{
			ID:        nodeUUID,
			Namespace: s.cfg.Namespace,
			OwnerID:   ownerUUID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return db.FleetNode{}, ErrNotFound
			}
			return db.FleetNode{}, fmt.Errorf("load node: %w", err)
		}
		return node, nil
	}
	if instanceID == "" {
		return db.FleetNode{}, fmt.Errorf("%w: instance_id or node_id is required", ErrInvalidRequest)
	}
	node, err := s.store.Queries().GetFleetNodeByContainerID(ctx, db.GetFleetNodeByContainerIDParams{
		ContainerID: instanceID,
		Namespace:   s.cfg.Namespace,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.FleetNode{}, ErrNotFound
		}
		return db.FleetNode{}, fmt.Errorf("load node: %w", err)
	}
	if store.UUIDText(node.OwnerID) != ownerID {
		return db.FleetNode{}, ErrNotFound
	}
	return node, nil
}

func (s *Service) applyOperation(ctx context.Context, node db.FleetNode, operation string) (db.FleetNode, error) {
	switch operation {
	case "status":
		return s.applyStatus(ctx, node)
	case "start":
		if node.ContainerID == "" {
			return db.FleetNode{}, ErrNotFound
		}
		if err := s.provider.Start(ctx, node.ContainerID); err != nil {
			return db.FleetNode{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
		return s.setTargetState(ctx, node, StateStarting)
	case "stop":
		if node.ContainerID == "" {
			return db.FleetNode{}, ErrNotFound
		}
		if err := s.provider.Stop(ctx, node.ContainerID, operationStopTimeout); err != nil {
			return db.FleetNode{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
		return s.setTargetState(ctx, node, StateStopping)
	case "reboot":
		if node.ContainerID == "" {
			return db.FleetNode{}, ErrNotFound
		}
		if err := s.provider.Restart(ctx, node.ContainerID, operationStopTimeout); err != nil {
			return db.FleetNode{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
		return s.setTargetState(ctx, node, StateRebooting)
	case "delete":
		return s.deleteNode(ctx, node)
	default:
		return db.FleetNode{}, fmt.Errorf("%w: unknown operation %q", ErrInvalidRequest, operation)
	}
}

func (s *Service) applyStatus(ctx context.Context, node db.FleetNode) (db.FleetNode, error) {
	if node.ContainerID == "" {
		// A failed launch never created a container. Status reports the
		// persisted record instead of pretending the node does not exist.
		return node, nil
	}
	observed, err := s.provider.Inspect(ctx, node.ContainerID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			// Only a confirmed missing container becomes instance_missing;
			// an unreachable daemon is uncertain and must not clear state.
			detail := node.StatusDetail
			if updateErr := s.store.Queries().UpdateFleetNodeObservedState(ctx, db.UpdateFleetNodeObservedStateParams{
				ID:            node.ID,
				Namespace:     s.cfg.Namespace,
				ObservedState: StateInstanceMissing,
				StatusDetail:  detail,
				Error:         sanitize(err),
			}); updateErr != nil {
				return db.FleetNode{}, fmt.Errorf("record missing instance: %w", updateErr)
			}
			return s.reloadNode(ctx, node.ID)
		}
		return db.FleetNode{}, fmt.Errorf("%w: inspect container: %v", ErrStateUncertain, err)
	}

	daemonReady, healthFresh := false, false
	// Keep the last known detail when no fresh health sample is readable; an
	// unavailable daemon must not erase what was observed before.
	detail := node.StatusDetail
	if observed.State == "running" {
		if report, healthErr := s.Diagnose(ctx, node); healthErr == nil {
			daemonReady = report.Status == "running"
			healthFresh = true
			detail = statusDetailFromHealth(report)
		}
	}
	display := DeriveDisplayState(node.TargetState, Observed{
		ContainerExists: true,
		ContainerState:  observed.State,
		Healthy:         observed.Healthy,
		DaemonReady:     daemonReady,
		HealthFresh:     healthFresh,
	})
	if err := s.store.Queries().UpdateFleetNodeObservedState(ctx, db.UpdateFleetNodeObservedStateParams{
		ID:            node.ID,
		Namespace:     s.cfg.Namespace,
		ObservedState: display,
		DaemonReady:   daemonReady,
		StatusDetail:  detail,
		Error:         "",
	}); err != nil {
		return db.FleetNode{}, fmt.Errorf("record node status: %w", err)
	}
	return s.reloadNode(ctx, node.ID)
}

// deleteNode revokes the credential first so a stopped token cannot be used
// while the container and its volumes are being removed.
func (s *Service) deleteNode(ctx context.Context, node db.FleetNode) (db.FleetNode, error) {
	if err := s.store.RevokeCredentials(ctx, node.ID); err != nil {
		return db.FleetNode{}, fmt.Errorf("revoke node credentials: %w", err)
	}
	if node.ContainerID != "" {
		if err := s.provider.Remove(ctx, node.ContainerID, false); err != nil {
			return db.FleetNode{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
	}
	if node.ContainerName != "" {
		for _, volume := range []string{node.ContainerName + "-data", node.ContainerName + "-secrets"} {
			if err := s.provider.RemoveVolume(ctx, volume); err != nil {
				return db.FleetNode{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
			}
		}
	}
	return s.setTargetState(ctx, node, StateTerminated)
}

func (s *Service) setTargetState(ctx context.Context, node db.FleetNode, target string) (db.FleetNode, error) {
	if err := s.store.Queries().UpdateFleetNodeTargetState(ctx, db.UpdateFleetNodeTargetStateParams{
		ID:          node.ID,
		Namespace:   s.cfg.Namespace,
		TargetState: target,
	}); err != nil {
		return db.FleetNode{}, fmt.Errorf("update node target state: %w", err)
	}
	return s.reloadNode(ctx, node.ID)
}

func (s *Service) reloadNode(ctx context.Context, nodeID pgtype.UUID) (db.FleetNode, error) {
	node, err := s.store.Queries().GetFleetNode(ctx, db.GetFleetNodeParams{
		ID: nodeID, Namespace: s.cfg.Namespace,
	})
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("reload node: %w", err)
	}
	return node, nil
}

// launchNode creates the namespace resources and the node container, seeds both
// volumes, then starts the node. On any failure it removes only the resources it
// created for this freshly reserved node id.
func (s *Service) launchNode(ctx context.Context, node db.FleetNode, profile Profile, token string) error {
	var spec Spec
	if err := json.Unmarshal(node.Spec, &spec); err != nil {
		return fmt.Errorf("decode node spec: %w", err)
	}
	containerName := nodeContainerPrefix + s.cfg.Namespace + "-" + store.UUIDText(node.ID)
	dataVolume := containerName + "-data"
	secretsVolume := containerName + "-secrets"
	labels := map[string]string{
		docker.LabelNamespace: s.cfg.Namespace,
		docker.LabelNode:      node.DaemonID,
		docker.LabelRole:      docker.RoleNode,
	}
	if err := s.provider.EnsureNetwork(ctx, s.cfg.NodeNetwork, labels); err != nil {
		return err
	}
	for _, volume := range []string{dataVolume, secretsVolume} {
		if err := s.provider.EnsureVolume(ctx, volume, labels); err != nil {
			s.cleanupLaunch(ctx, "", dataVolume, secretsVolume)
			return err
		}
	}

	// Secrets go to the read-only secrets volume through PrepareVolumes, which
	// must run before the node exists because the node cannot write that mount.
	files := map[string][]byte{
		"config.json": []byte(buildCLIConfig(s.cfg.APIURL, s.cfg.APIURL, token)),
		"env":         []byte(buildEnvFile(profile.Env)),
	}
	if err := s.provider.PrepareVolumes(ctx, dataVolume, secretsVolume, s.cfg.RuntimeImage, files); err != nil {
		s.cleanupLaunch(ctx, "", dataVolume, secretsVolume)
		return err
	}

	nodeSpec := docker.BuildNodeSpec(node.DaemonID, s.cfg.Namespace, containerName, s.cfg.RuntimeImage,
		dataVolume, secretsVolume, s.cfg.NodeNetwork, spec.MemoryMB, spec.CPU, spec.PidsLimit)
	nodeSpec.Env = []string{
		"MULTICA_SERVER_URL=" + s.cfg.APIURL,
		"MULTICA_DAEMON_ID=" + node.DaemonID,
		"MULTICA_DAEMON_DEVICE_NAME=" + node.Name,
		"MULTICA_WORKSPACES_ROOT=/home/multica/workspaces",
		"MULTICA_DAEMON_AUTO_UPDATE=false",
		"MULTICA_DAEMON_AUTO_RELOAD=false",
		"HOME=/home/multica",
	}
	containerID, err := s.provider.CreateContainer(ctx, nodeSpec)
	if err != nil {
		s.cleanupLaunch(ctx, "", dataVolume, secretsVolume)
		return err
	}
	if err := s.provider.Start(ctx, containerID); err != nil {
		s.cleanupLaunch(ctx, containerID, dataVolume, secretsVolume)
		return err
	}
	if err := s.store.Queries().UpdateFleetNodeContainer(ctx, db.UpdateFleetNodeContainerParams{
		ID:            node.ID,
		Namespace:     s.cfg.Namespace,
		ContainerID:   containerID,
		ContainerName: containerName,
	}); err != nil {
		s.cleanupLaunch(ctx, containerID, dataVolume, secretsVolume)
		return fmt.Errorf("record node container: %w", err)
	}
	return nil
}

func (s *Service) cleanupLaunch(ctx context.Context, containerID string, volumes ...string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), launchCleanupTimeout)
	defer cancel()
	if containerID != "" {
		_ = s.provider.Remove(cleanupCtx, containerID, false)
	}
	for _, volume := range volumes {
		_ = s.provider.RemoveVolume(cleanupCtx, volume)
	}
}

// failNode records a create failure and revokes the credential minted for it.
// It uses its own bounded context so a canceled request still records the
// failure.
func (s *Service) failNode(ctx context.Context, node db.FleetNode, cause error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureWriteTimeout)
	defer cancel()
	_ = s.store.RevokeCredentials(cleanupCtx, node.ID)
	_ = s.store.Queries().UpdateFleetNodeTargetState(cleanupCtx, db.UpdateFleetNodeTargetStateParams{
		ID:          node.ID,
		Namespace:   s.cfg.Namespace,
		TargetState: StateFailed,
		Error:       sanitize(cause),
	})
}

func mintNodeToken() (plain, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("mint node token: %w", err)
	}
	plain = auth.CloudPATPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return plain, auth.HashToken(plain), nil
}

// buildCLIConfig renders the node's Multica CLI config. It contains the node
// token and the container-reachable URLs only; model credentials go to the
// separate env file.
func buildCLIConfig(serverURL, appURL, token string) string {
	cfg := struct {
		ServerURL      string `json:"server_url"`
		AppURL         string `json:"app_url"`
		Token          string `json:"token"`
		WorkspacesRoot string `json:"workspaces_root"`
	}{
		ServerURL:      serverURL,
		AppURL:         appURL,
		Token:          token,
		WorkspacesRoot: "/home/multica/workspaces",
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		// The struct holds strings only, so encoding cannot fail.
		return ""
	}
	return string(encoded)
}

// buildEnvFile renders the model credential env in a deterministic order.
func buildEnvFile(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteString("=")
		builder.WriteString(env[key])
		builder.WriteString("\n")
	}
	return builder.String()
}

// clampOffset keeps a caller-supplied offset inside the non-negative int32
// range the SQL query binds, so an oversized or negative value cannot become a
// negative or wrapped OFFSET.
func clampOffset(offset int) int32 {
	switch {
	case offset < 0:
		return 0
	case offset > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(offset)
	}
}

// sanitize bounds an error string before it is stored, so a Docker error cannot
// grow the row or carry an unbounded payload. The limit counts runes so a
// multibyte message is never cut into invalid UTF-8.
func sanitize(err error) string {
	if err == nil {
		return ""
	}
	const maxRunes = 300
	runes := []rune(err.Error())
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return string(runes)
}
