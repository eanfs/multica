# Aurora Sandbox Fleet Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Autoprovision and retire one hardened Docker sandbox node per Aurora workspace, with authenticated fleet control, enforced outbound proxying, bounded resources, and Linux-verifiable isolation.

**Architecture:** The Multica server issues a scoped enrollment through child plan A and calls an authenticated fleet control API to ensure a workspace node. The Docker backend creates a workspace-private internal network, an egress-proxy sidecar attached to a separate uplink network, and a digest-pinned sandbox container with immutable security flags and file-mounted secrets. A server-side lifecycle manager reconciles starting, online, draining, failed, and stopped nodes; it refunds queued/running generations through the existing task failure settlement path when infrastructure fails.

**Tech Stack:** Go 1.26.6, Chi, Docker Engine CLI/API semantics, PostgreSQL/sqlc node state from child plan A, custom HTTP CONNECT proxy, Linux seccomp/AppArmor/cgroups

**Spec:** `docs/superpowers/specs/2026-09-11-aurora-content-creation-app-design.md`

## Global Constraints

- This plan is child plan B of `docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md` and consumes child plan A’s `SandboxEnrollmentService` and node table.
- Exactly one sandbox container and one egress sidecar represent a workspace node. The sidecar does not count as a second execution node and cannot claim tasks.
- Fleet control binds to loopback by default. A non-loopback bind requires HTTPS and a configured server certificate; plain HTTP on a non-loopback address is a startup error.
- Server-to-fleet authentication uses a random credential read from `AURORA_FLEET_CONTROL_TOKEN_FILE`. The value is never accepted directly in an environment variable or logged.
- The fleet chooses `AURORA_SANDBOX_IMAGE`; API callers cannot supply or override an image, command, security option, mount, environment variable, label namespace, network, or resource limit.
- `AURORA_SANDBOX_IMAGE` must be an OCI reference containing an immutable `@sha256:<64 lowercase hex>` digest. The backend uses `--pull never`; image acquisition is an operator/deployment step.
- Sandbox UID/GID is `10001:10001`; root filesystem is read-only; all capabilities are dropped; `no-new-privileges`, seccomp, and AppArmor are mandatory on Linux.
- Sandbox limits are 2 CPUs, 4 GiB memory, memory swap equal to memory, 256 PIDs, 1,024 open files, 2 GiB `/workspace` tmpfs, 256 MiB `/tmp` tmpfs, and 16 MiB `/run` tmpfs.
- Every writable tmpfs is `nosuid,nodev,noexec`; files that must execute are installed in the read-only image.
- The sandbox receives no Docker socket, host filesystem mount, host device, privileged mode, or host PID/IPC/network namespace.
- The sandbox’s only network is a per-workspace Docker network created with `--internal`. Only its egress sidecar also joins the fleet uplink network.
- The sandbox can reach the egress sidecar at `http://egress:3128`; all direct public/private/metadata network paths must fail.
- The proxy permits only the exact Multica server origin and compiled provider hosts `api.anthropic.com:443`, `ark.cn-beijing.volces.com:443`, `api.openai.com:443`, and `openspeech.bytedance.com:443`, plus exact hosts explicitly configured by the operator. It never permits wildcard `*` or arbitrary ports.
- Volcengine media output hosts are intentionally not allowlisted because the official skills return arbitrary response URLs. Child plan C imports those URLs through a server-controlled SSRF-safe fetch path.
- Security acceptance runs on Linux Docker Engine. Docker Desktop on macOS is a functional development smoke only.
- Infrastructure failure settles tasks through `TaskService.HandleFailedTasks`; direct task-row updates and generic cancellation are forbidden because they can strand Aurora credit reservations.

## File Structure

### New files

- `server/internal/aurorafleet/auth.go` / `_test.go` — constant-time bearer validation from a credential file.
- `server/internal/aurorafleet/policy.go` / `_test.go` — immutable sandbox/proxy policy and deterministic Docker arguments.
- `server/internal/aurorafleet/client.go` / `_test.go` — server-side typed fleet control client.
- `server/internal/aurorafleet/reconcile.go` / `_test.go` — label-based startup reconciliation and rollback.
- `server/internal/aurora/sandbox_manager.go` / `_test.go` — workspace ensure operation around child plan A enrollment.
- `server/internal/aurora/sandbox_reaper.go` / `_test.go` — starting timeout, idle TTL, hard-lifetime drain, stop, revoke, and task settlement.
- `server/internal/auroraegress/policy.go` / `_test.go` — exact-host/port, DNS/IP, redirect, and server-origin policy.
- `server/internal/auroraegress/proxy.go` / `_test.go` — HTTP and CONNECT proxy.
- `server/cmd/aurora-egress-proxy/main.go` — sidecar entry point.
- `deploy/aurora-sandbox/seccomp.json` — explicit syscall allow/deny policy tested against the image.
- `deploy/aurora-sandbox/multica-aurora-sandbox.apparmor` — Linux AppArmor profile.
- `deploy/aurora-sandbox/docker-smoke.sh` — functional Docker Desktop smoke with fake services.
- `deploy/aurora-sandbox/docker-security-test.sh` — Linux-only container inspection and adversarial network/filesystem checks.
- `server/internal/aurorafleet/docker_integration_test.go` — opt-in Linux Docker lifecycle test.

### Modified files

- `server/internal/aurorafleet/backend.go` — replace caller-controlled request with typed workspace node policy.
- `server/internal/aurorafleet/docker.go` / `_test.go` — create network, proxy, sandbox, health wait, rollback, inspect, and delete.
- `server/internal/aurorafleet/controller.go` / `_test.go` — authenticated internal ensure/status/delete routes; remove arbitrary exec/create surface from the managed API.
- `server/cmd/aurora-fleet/main.go` — parse file-based auth, policy, images, networks, TLS, TTLs, and secret paths.
- `server/internal/handler/aurora.go` / `_test.go` — ensure infrastructure before reserve/enqueue.
- `server/internal/handler/handler.go` — inject `WorkspaceSandboxManager`.
- `server/cmd/server/main.go` — construct fleet client/manager/reaper.
- `server/cmd/server/runtime_sweeper.go` / `_test.go` — run node lifecycle sweep beside runtime expiry.
- `server/pkg/db/queries/aurora_sandbox_node.sql` — fleet backend ID updates and lifecycle candidates.
- `.env.example` — document fleet URL, token-file path, digest image, TTL, and proxy origin.

## Public Interfaces

```go
// server/internal/aurorafleet/backend.go
type WorkspaceNodeSpec struct {
    NodeID          string
    WorkspaceID     string
    RuntimeID       string
    DaemonID        string
    EnrollmentFile string
}

type Node struct {
    ID            string `json:"id"`
    ProxyID       string `json:"proxy_id"`
    NetworkID     string `json:"network_id"`
    State         string `json:"state"`
    Health        string `json:"health"`
}

type Backend interface {
    EnsureWorkspaceNode(ctx context.Context, spec WorkspaceNodeSpec) (Node, error)
    WorkspaceNodeStatus(ctx context.Context, nodeID string) (Node, error)
    DeleteWorkspaceNode(ctx context.Context, nodeID string) error
    Reconcile(ctx context.Context) error
}
```

```go
// server/internal/aurorafleet/client.go
type EnsureRequest struct {
    NodeID          string `json:"node_id"`
    WorkspaceID     string `json:"workspace_id"`
    RuntimeID       string `json:"runtime_id"`
    DaemonID        string `json:"daemon_id"`
    EnrollmentToken string `json:"enrollment_token"`
}

type ControlClient interface {
    EnsureWorkspaceNode(ctx context.Context, req EnsureRequest) (Node, error)
    WorkspaceNodeStatus(ctx context.Context, nodeID string) (Node, error)
    DeleteWorkspaceNode(ctx context.Context, nodeID string) error
}
```

```go
// server/internal/aurora/sandbox_manager.go
type WorkspaceSandboxManager interface {
    Ensure(ctx context.Context, workspaceID, runtimeID pgtype.UUID) (db.AuroraSandboxNode, error)
}
```

```go
// server/internal/auroraegress/policy.go
type Policy struct {
    ServerOrigin    *url.URL
    AllowedTLSHosts map[string]struct{}
    Resolve         func(context.Context, string) ([]net.IPAddr, error)
}

func (p Policy) Authorize(target *url.URL, isConnect bool) error
```

---

### Task 1: Replace Arbitrary Fleet Node Control with an Authenticated Workspace API

**Files:**
- Create: `server/internal/aurorafleet/auth.go`
- Create: `server/internal/aurorafleet/auth_test.go`
- Create: `server/internal/aurorafleet/client.go`
- Create: `server/internal/aurorafleet/client_test.go`
- Modify: `server/internal/aurorafleet/backend.go`
- Modify: `server/internal/aurorafleet/controller.go`
- Modify: `server/internal/aurorafleet/controller_test.go`
- Modify: `server/cmd/aurora-fleet/main.go`

**Interfaces:**
- Consumes: Child plan A’s node/runtime/daemon IDs and `mse_` token.
- Produces: Authenticated `PUT /internal/v1/workspace-nodes/{nodeID}`, `GET` status, and `DELETE` operations plus `ControlClient`.

- [ ] **Step 1: Write failing authentication and shape tests**

Cover missing/incorrect bearer, malformed JSON, path/body node mismatch, arbitrary extra fields, valid ensure, status, and delete. Assert the old generic `/api/v1/nodes/exec` and caller-selected image create route return 404 from the managed controller.

```go
func TestWorkspaceNodeEnsureRequiresControlBearer(t *testing.T)
func TestWorkspaceNodeEnsureRejectsIdentityMismatch(t *testing.T)
func TestWorkspaceNodeEnsureDoesNotAcceptImageOrCommand(t *testing.T)
func TestWorkspaceNodeRoutesCallTypedBackend(t *testing.T)
func TestLegacyFleetExecRouteIsNotExposed(t *testing.T)
```

- [ ] **Step 2: Run focused tests and observe the current open API**

Run:

```bash
cd server && go test ./internal/aurorafleet -run 'TestWorkspaceNode|TestLegacyFleetExec' -count=1
```

Expected: tests fail because the current controller exposes unauthenticated generic node operations.

- [ ] **Step 3: Implement file-based control authentication**

At process startup, read a regular, non-symlink token file with mode `0400` or `0600`, maximum 256 bytes, and at least 32 random bytes represented as base64url or hex. Store the trimmed value only in memory. Compare bearer bytes with `subtle.ConstantTimeCompare`; return the same 401 response for missing and wrong tokens. Exclude `/healthz` from auth and require auth for `/readyz` and every internal node route.

- [ ] **Step 4: Replace the managed route surface**

Expose only:

```text
PUT    /internal/v1/workspace-nodes/{nodeID}
GET    /internal/v1/workspace-nodes/{nodeID}
DELETE /internal/v1/workspace-nodes/{nodeID}
GET    /healthz
GET    /readyz
```

The ensure request contains exactly `node_id`, `workspace_id`, `runtime_id`, `daemon_id`, and `enrollment_token`; JSON decoding uses `DisallowUnknownFields`, enforces a 4 KiB body cap, and validates UUID/token formats. It does not accept an image reference, environment map, labels, mounts, command, or resource policy.

- [ ] **Step 5: Write the enrollment secret to a controlled host file**

The controller creates `<secret-root>/<nodeID>/enrollment` with parent mode `0700`, file mode `0400`, exclusive creation, and atomic rename. It passes only the absolute file path to `Backend.EnsureWorkspaceNode`; it removes the file after successful enrollment is observed, on rollback, and on node deletion. It never returns or logs the token.

- [ ] **Step 6: Implement the typed server client**

`ControlClient` reads its bearer token from the configured file at construction, uses a 10-second connect timeout and 60-second request deadline, disables redirects, caps response bodies at 1 MiB, and reports status/message without echoing request secrets. It requires HTTPS for non-loopback URLs.

- [ ] **Step 7: Run controller/client tests**

Run:

```bash
cd server && go test ./internal/aurorafleet -run 'TestWorkspaceNode|TestLegacyFleetExec|TestControlClient|TestControlAuth' -count=1
```

Expected: all selected tests pass.

- [ ] **Step 8: Commit the control API**

```bash
git add server/internal/aurorafleet/auth.go server/internal/aurorafleet/auth_test.go \
  server/internal/aurorafleet/client.go server/internal/aurorafleet/client_test.go \
  server/internal/aurorafleet/backend.go server/internal/aurorafleet/controller.go \
  server/internal/aurorafleet/controller_test.go server/cmd/aurora-fleet/main.go
git commit -m "feat(aurora): authenticate workspace fleet control"
```

### Task 2: Encode an Immutable Hardened Docker Policy

**Files:**
- Create: `server/internal/aurorafleet/policy.go`
- Create: `server/internal/aurorafleet/policy_test.go`
- Create: `deploy/aurora-sandbox/seccomp.json`
- Create: `deploy/aurora-sandbox/multica-aurora-sandbox.apparmor`
- Modify: `server/internal/aurorafleet/docker.go`
- Modify: `server/internal/aurorafleet/docker_test.go`

**Interfaces:**
- Consumes: `WorkspaceNodeSpec`, process configuration for digest-pinned sandbox/proxy images, trusted secret-file paths, and server origin.
- Produces: Deterministic Docker CLI arguments that callers cannot weaken.

- [ ] **Step 1: Write exact argument-policy tests**

Assert the sandbox command contains all required flags and none of the forbidden flags:

```go
func TestSandboxArgsEnforceImmutablePolicy(t *testing.T) {
    args := policy.SandboxArgs(spec)
    require.Subset(t, args, []string{
        "--user", "10001:10001", "--read-only", "--cap-drop", "ALL",
        "--security-opt", "no-new-privileges:true", "--pids-limit", "256",
        "--memory", "4g", "--memory-swap", "4g", "--cpus", "2",
        "--ulimit", "nofile=1024:1024", "--pull", "never", "--init",
    })
    require.NotContains(t, strings.Join(args, " "), "--privileged")
    require.NotContains(t, strings.Join(args, " "), "/var/run/docker.sock")
}
```

Also test tag-only images, uppercase/malformed digests, relative secret paths, symlink secret paths, paths outside the configured secret root, duplicate labels, and workspace-controlled strings containing Docker flags.

- [ ] **Step 2: Run policy tests and observe missing enforcement**

Run:

```bash
cd server && go test ./internal/aurorafleet -run 'TestSandboxArgs|TestProxyArgs|TestImageDigest|TestSecretMount' -count=1
```

Expected: tests fail because the current backend constructs `docker run` from caller maps and lacks hardening.

- [ ] **Step 3: Define immutable sandbox arguments**

Generate this policy in code, not request data:

```text
run --detach --pull never --init --restart no --stop-timeout 30
--user 10001:10001 --read-only --cap-drop ALL
--security-opt no-new-privileges:true
--security-opt seccomp=<absolute trusted seccomp path>
--security-opt apparmor=multica-aurora-sandbox
--pids-limit 256 --memory 4g --memory-swap 4g --cpus 2
--ulimit nofile=1024:1024
--tmpfs /workspace:rw,nosuid,nodev,noexec,size=2147483648,uid=10001,gid=10001,mode=0700
--tmpfs /tmp:rw,nosuid,nodev,noexec,size=268435456,uid=10001,gid=10001,mode=0700
--tmpfs /run:rw,nosuid,nodev,noexec,size=16777216,uid=10001,gid=10001,mode=0755
--network <workspace internal network>
--mount type=bind,src=<trusted enrollment file>,dst=/run/secrets/aurora-enrollment,readonly
-e MULTICA_SERVER_URL=<configured origin>
-e MULTICA_MANAGED=1
-e MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE=/run/secrets/aurora-enrollment
-e MULTICA_AGENT_TIMEOUT=30m
-e HTTP_PROXY=http://egress:3128
-e HTTPS_PROXY=http://egress:3128
-e NO_PROXY=egress,127.0.0.1,localhost
<digest-pinned image>
```

Add only controlled `com.multica.aurora.*` labels containing node/workspace/runtime/daemon identity. Never put a token, prompt, user name, provider URL, or provider credential in a label/environment value.

- [ ] **Step 4: Define proxy container policy**

The proxy sidecar runs non-root, read-only, capability-free, no-new-privileges, 0.25 CPU, 256 MiB memory/swap, 64 PIDs, 512 open files, and a 32 MiB `/tmp` tmpfs. It mounts no enrollment/provider credentials. It receives the exact server origin and allowed host list as non-secret configuration, listens only on the workspace internal network, and then joins the fleet uplink network.

- [ ] **Step 5: Add explicit seccomp and AppArmor policy files**

Start from Docker’s current default seccomp allowlist, retain syscalls proven necessary by Claude/Node/Chromium/FFmpeg smoke, and explicitly deny mount, umount, pivot_root, ptrace, bpf, perf_event_open, keyctl, add_key, request_key, kexec_load, init_module, finit_module, delete_module, swapon, swapoff, reboot, and namespace-changing clone/unshare flags. The AppArmor profile denies raw network, mount, `/proc/*/mem`, kernel/sys writes, Docker socket paths, and all host paths except the read-only enrollment/provider secret files named in the deployment.

The implementation task must validate profile syntax with `apparmor_parser -Q` on Linux before loading it.

- [ ] **Step 6: Replace map-driven Docker run construction**

Delete `CreateRequest.Env`, caller labels, and caller image selection from the managed path. Build every name from a lowercase SHA-256 prefix of workspace ID plus node ID; validate length and character set before invoking Docker with an argv slice.

- [ ] **Step 7: Run policy tests**

Run:

```bash
cd server && go test ./internal/aurorafleet -run 'TestSandboxArgs|TestProxyArgs|TestImageDigest|TestSecretMount' -count=1
```

Expected: all selected tests pass.

- [ ] **Step 8: Commit the policy**

```bash
git add server/internal/aurorafleet/policy.go server/internal/aurorafleet/policy_test.go \
  server/internal/aurorafleet/docker.go server/internal/aurorafleet/docker_test.go \
  deploy/aurora-sandbox/seccomp.json \
  deploy/aurora-sandbox/multica-aurora-sandbox.apparmor
git commit -m "feat(aurora): harden sandbox container policy"
```

### Task 3: Enforce Outbound Access Through a Narrow Proxy

**Files:**
- Create: `server/internal/auroraegress/policy.go`
- Create: `server/internal/auroraegress/policy_test.go`
- Create: `server/internal/auroraegress/proxy.go`
- Create: `server/internal/auroraegress/proxy_test.go`
- Create: `server/cmd/aurora-egress-proxy/main.go`
- Modify: `server/internal/aurorafleet/docker.go`
- Modify: `server/internal/aurorafleet/docker_test.go`

**Interfaces:**
- Consumes: Exact Multica server origin and exact provider hosts.
- Produces: HTTP/CONNECT proxy on `:3128` with DNS/IP validation, bounded tunnels, and privacy-safe audit logs.

- [ ] **Step 1: Write the authorization matrix first**

Use injected resolver/dialer fakes to cover:

```go
func TestPolicyAllowsCompiledProviderHTTPSHosts(t *testing.T)
func TestPolicyAllowsExactConfiguredServerOrigin(t *testing.T)
func TestPolicyRejectsHTTPForProviderHosts(t *testing.T)
func TestPolicyRejectsUnknownHostAndPort(t *testing.T)
func TestPolicyRejectsURLCredentialsAndIPLiteral(t *testing.T)
func TestPolicyRejectsPrivateLoopbackLinkLocalCGNATAndMetadataIPs(t *testing.T)
func TestPolicyRejectsMixedPublicAndPrivateDNSAnswers(t *testing.T)
func TestProxyPinsValidatedDNSAddressesForDial(t *testing.T)
func TestProxyBoundsHeadersBodiesAndTunnelLifetime(t *testing.T)
func TestProxyLogsHostWithoutPathQueryOrAuthorization(t *testing.T)
```

The server-origin exception may resolve privately, but only its exact scheme, host, and port are allowed. That exception cannot authorize a sibling port or another host resolving to the same IP.

- [ ] **Step 2: Run the proxy tests and observe missing package**

Run:

```bash
cd server && go test ./internal/auroraegress -count=1
```

Expected: package or symbols do not exist.

- [ ] **Step 3: Implement exact target parsing and DNS policy**

For provider CONNECT requests, require `host:443`, reject IP literals and URL credentials, resolve A/AAAA records, reject the complete answer if any address is non-public, and pass the validated addresses to the dialer without a second DNS lookup. Block loopback, RFC1918, link-local, CGNAT, multicast, documentation, benchmark, unspecified, and cloud metadata ranges for IPv4 and IPv6.

For the Multica server origin, permit only the configured origin’s exact host/port and only `http` or `https`; do not generalize its private-address exception.

- [ ] **Step 4: Implement bounded HTTP and CONNECT forwarding**

Limits:

- request headers: 32 KiB;
- request body: 8 MiB for normal HTTP control-plane requests;
- idle tunnel timeout: 90 seconds;
- total tunnel lifetime: 35 minutes;
- connect/dial timeout: 10 seconds;
- response header timeout: 30 seconds;
- maximum simultaneous tunnels per sidecar: 8.

Do not follow redirects inside the proxy. The requesting client handles provider API redirects, while the policy re-authorizes every new target. Strip `Proxy-Authorization`, `Forwarded`, and incoming `X-Forwarded-*`; add no user identity.

- [ ] **Step 5: Add the sidecar command**

Parse configuration once, reject wildcard hosts and non-provider ports, bind `0.0.0.0:3128` inside the internal network, expose `/healthz` on a separate loopback port, and shut down tunnels on context cancellation. Logs include decision, target host/port, byte counts, duration, and a generated connection ID only.

- [ ] **Step 6: Wire per-workspace networks and proxy alias**

The Docker backend must:

1. create `aurora-ws-<hash>` with `docker network create --internal`;
2. start proxy attached to that network with alias `egress`;
3. connect proxy to a pre-created `aurora-egress-uplink` bridge;
4. start sandbox attached only to the internal network;
5. roll back sandbox, proxy, network, and secret on any partial failure.

- [ ] **Step 7: Run proxy and Docker argument tests**

Run:

```bash
cd server && go test ./internal/auroraegress ./internal/aurorafleet -run 'TestPolicy|TestProxy|TestDocker.*Network|TestEnsure.*Rollback' -count=1
```

Expected: all selected tests pass with injected local fakes and no external requests.

- [ ] **Step 8: Commit enforced egress**

```bash
git add server/internal/auroraegress server/cmd/aurora-egress-proxy \
  server/internal/aurorafleet/docker.go server/internal/aurorafleet/docker_test.go
git commit -m "feat(aurora): enforce sandbox egress proxy"
```

### Task 4: Autoprovision Before Aurora Reserves Credits

**Files:**
- Create: `server/internal/aurora/sandbox_manager.go`
- Create: `server/internal/aurora/sandbox_manager_test.go`
- Modify: `server/internal/handler/aurora.go`
- Modify: `server/internal/handler/aurora_test.go`
- Modify: `server/internal/handler/handler.go`
- Modify: `server/cmd/server/main.go`
- Modify: `server/pkg/db/queries/aurora_sandbox_node.sql`

**Interfaces:**
- Consumes: Child plan A’s `SandboxEnrollmentService.Issue`, this plan’s `ControlClient`, and the seeded managed runtime.
- Produces: `WorkspaceSandboxManager.Ensure` and generation fail-closed behavior before reserve/enqueue.

- [ ] **Step 1: Write manager idempotency and rollback tests**

Cover:

```go
func TestSandboxManagerReturnsHealthyOnlineNodeWithoutFleetCall(t *testing.T)
func TestSandboxManagerIssuesEnrollmentAndCallsFleetOnce(t *testing.T)
func TestSandboxManagerConcurrentEnsureCreatesOneNode(t *testing.T)
func TestSandboxManagerMarksNodeFailedWhenFleetRejects(t *testing.T)
func TestSandboxManagerDoesNotReturnEnrollmentToken(t *testing.T)
```

- [ ] **Step 2: Write generation charge-order regressions**

Add handler tests that instrument manager, credit service, and enqueue calls:

```go
func TestCreateGenerationEnsuresSandboxBeforeReserve(t *testing.T)
func TestCreateGenerationFleetDisabledReturns503WithoutReserve(t *testing.T)
func TestCreateGenerationFleetFailureReturns503WithoutReserveOrEnqueue(t *testing.T)
```

Expected response code is 503 with stable code `aurora_runtime_unavailable`.

- [ ] **Step 3: Run the focused tests and observe missing manager**

Run:

```bash
source .env.worktree
cd server && go test ./internal/aurora ./internal/handler -run 'TestSandboxManager|TestCreateGeneration.*Sandbox|TestCreateGenerationFleet' -count=1
```

Expected: compilation or behavior failure because generation currently reserves/enqueues without fleet ensure.

- [ ] **Step 4: Implement idempotent ensure**

Under the same workspace advisory lock used by enrollment issuance:

1. load the managed runtime and node;
2. return an `online` node whose runtime/daemon/image digest still match;
3. return a recent `starting` node without issuing another token;
4. issue/rotate enrollment for no row, stopped, failed, or stale starting row;
5. call fleet ensure after the transaction commits;
6. persist returned backend node ID;
7. if the fleet call fails, mark the node failed and clear enrollment fields.

Never hold a database transaction across an HTTP fleet call.

- [ ] **Step 5: Reorder generation creation**

The handler flow becomes:

```text
parse + authorize + catalog/input validation + prompt moderation
ensure Aurora system agents/runtime
ensure workspace sandbox accepted by fleet
transactional entitlement check + generation row
credit reserve
quick-create enqueue
```

A node may be created for a request later rejected by an entitlement check; the idle reaper removes it. This trade-off avoids reserving credits for infrastructure that could not start and avoids network I/O under the entitlement transaction.

- [ ] **Step 6: Wire server configuration fail-closed**

Construct the fleet client only when both `AURORA_FLEET_URL` and `AURORA_FLEET_CONTROL_TOKEN_FILE` are valid. If either is absent, expose the catalog/library normally but reject new generation creation before reserve. Do not fall back to queued-without-runtime behavior.

- [ ] **Step 7: Run focused tests**

Run:

```bash
source .env.worktree
cd server && go test ./internal/aurora ./internal/handler -run 'TestSandboxManager|TestCreateGeneration.*Sandbox|TestCreateGenerationFleet' -count=1
```

Expected: all selected tests pass.

- [ ] **Step 8: Commit autoprovisioning**

```bash
git add server/internal/aurora/sandbox_manager.go server/internal/aurora/sandbox_manager_test.go \
  server/internal/handler/aurora.go server/internal/handler/aurora_test.go \
  server/internal/handler/handler.go server/cmd/server/main.go \
  server/pkg/db/queries/aurora_sandbox_node.sql server/pkg/db/generated
git commit -m "feat(aurora): autoprovision workspace sandboxes"
```

### Task 5: Reap Starting, Idle, Hard-Expired, and Orphaned Nodes

**Files:**
- Create: `server/internal/aurora/sandbox_reaper.go`
- Create: `server/internal/aurora/sandbox_reaper_test.go`
- Create: `server/internal/aurorafleet/reconcile.go`
- Create: `server/internal/aurorafleet/reconcile_test.go`
- Modify: `server/cmd/server/runtime_sweeper.go`
- Modify: `server/cmd/server/runtime_sweeper_test.go`
- Modify: `server/pkg/db/queries/aurora_sandbox_node.sql`
- Modify: `server/internal/aurorafleet/docker.go`

**Interfaces:**
- Consumes: `ControlClient`, node lifecycle rows, task service failure settlement, and daemon-token cache invalidation.
- Produces: 30-second node sweep and fleet startup reconciliation.

- [ ] **Step 1: Write a clock-driven lifecycle matrix**

Use an injected clock and fake fleet client:

```go
func TestSandboxReaperFailsStartingNodeAfterTwoMinutes(t *testing.T)
func TestSandboxReaperKeepsIdleNodeBeforeFifteenMinutes(t *testing.T)
func TestSandboxReaperStopsIdleNodeAfterFifteenMinutes(t *testing.T)
func TestSandboxReaperMarksEightHourNodeDraining(t *testing.T)
func TestSandboxReaperKeepsDrainingNodeWithActiveTask(t *testing.T)
func TestSandboxReaperFailsTaskAndStopsAfterThirtyMinuteDrain(t *testing.T)
func TestSandboxReaperRevokesTokensAndMarksRuntimeOffline(t *testing.T)
func TestSandboxReaperUsesAuroraFailureSettlementForRefund(t *testing.T)
func TestFleetReconcileDeletesOrphanedLabeledContainers(t *testing.T)
func TestFleetReconcileNeverTouchesUnlabeledContainers(t *testing.T)
```

- [ ] **Step 2: Run tests and observe missing lifecycle worker**

Run:

```bash
source .env.worktree
cd server && go test ./internal/aurora ./internal/aurorafleet ./cmd/server -run 'TestSandboxReaper|TestFleetReconcile' -count=1
```

Expected: compilation fails because the reaper/reconciler do not exist.

- [ ] **Step 3: Implement state-specific actions**

Every 30 seconds, process at most 100 candidates:

- `starting` older than 2 minutes without enrollment: delete fleet node, mark failed, fail its queued Aurora tasks with `runtime_start_failed`, and pass returned tasks to `HandleFailedTasks` for refund.
- `online` with no queued/running task and `last_active_at < now-15m`: delete node, revoke tokens, mark runtime offline, mark node stopped.
- any node whose current `started_at` is older than 8 hours: mark draining; generation ensure must not reuse it.
- `draining` with no active task: stop immediately.
- `draining` for 30 minutes with an active task: fail the task through the task service with `runtime_lifetime_exceeded`, settle/refund, then stop.

Use one responder for task failure; do not combine sweeper and WebSocket settlement paths.

- [ ] **Step 4: Make cleanup idempotent**

Treat Docker/container/network not-found as successful deletion. Token deletion, runtime offline update, and node stopped update run in one database transaction. Repeated sweeps must not issue duplicate refunds because the existing ledger uses idempotency keys and the task transition returns only newly failed tasks.

- [ ] **Step 5: Reconcile labeled Docker resources on fleet startup**

List only resources with `com.multica.aurora.managed=true`. Compare their node IDs with server-provided desired state or a configured startup grace cache; remove partial proxy/network resources whose sandbox is absent and remove complete nodes not recognized by the server after the reconciliation grace. Never enumerate/delete unlabeled user containers or networks.

- [ ] **Step 6: Wire the sweeper and run lifecycle tests**

Run:

```bash
source .env.worktree
cd server && go test ./internal/aurora ./internal/aurorafleet ./cmd/server -run 'TestSandboxReaper|TestFleetReconcile' -count=1
```

Expected: all selected tests pass.

- [ ] **Step 7: Commit lifecycle cleanup**

```bash
git add server/internal/aurora/sandbox_reaper.go server/internal/aurora/sandbox_reaper_test.go \
  server/internal/aurorafleet/reconcile.go server/internal/aurorafleet/reconcile_test.go \
  server/internal/aurorafleet/docker.go server/cmd/server/runtime_sweeper.go \
  server/cmd/server/runtime_sweeper_test.go server/pkg/db/queries/aurora_sandbox_node.sql \
  server/pkg/db/generated
git commit -m "feat(aurora): reap managed sandbox nodes"
```

### Task 6: Prove Docker Isolation on Linux

**Files:**
- Create: `server/internal/aurorafleet/docker_integration_test.go`
- Create: `deploy/aurora-sandbox/docker-security-test.sh`
- Modify: `server/internal/aurorafleet/docker.go`
- Modify: `server/internal/auroraegress/proxy_test.go`

**Interfaces:**
- Consumes: Tasks 1–5 and a locally built fixture image containing the managed daemon/proxy entrypoints.
- Produces: `auroradocker`-tagged Linux acceptance test and machine-readable inspection evidence.

- [ ] **Step 1: Add an explicitly gated Docker integration test**

Use build tag `auroradocker` and require `AURORA_RUN_DOCKER_SECURITY_TEST=1` before any Docker lookup. The test creates unique labeled resources and always removes them with `t.Cleanup`.

```go
//go:build auroradocker

func TestDockerSandboxLinuxSecurityBoundary(t *testing.T) {
    if os.Getenv("AURORA_RUN_DOCKER_SECURITY_TEST") != "1" {
        t.Skip("set AURORA_RUN_DOCKER_SECURITY_TEST=1")
    }
    require.Equal(t, "linux", runtime.GOOS)
    // ensure node, inspect flags, run adversarial probes, delete, assert no resources
}
```

- [ ] **Step 2: Make the first run fail on the unproven boundary**

Run on Linux:

```bash
cd server && AURORA_RUN_DOCKER_SECURITY_TEST=1 go test -tags=auroradocker ./internal/aurorafleet -run TestDockerSandboxLinuxSecurityBoundary -count=1 -v
```

Expected before fixture/image wiring: FAIL with a specific missing image/profile/resource assertion, not a silent skip.

- [ ] **Step 3: Inspect every required container field**

The test parses `docker inspect` JSON and asserts:

- user `10001:10001`;
- read-only root;
- capability drop contains `ALL` and capability add is empty;
- `SecurityOpt` contains no-new-privileges, seccomp path/profile, and AppArmor profile;
- memory `4294967296`, memory swap `4294967296`, nano CPUs `2000000000`, PIDs `256`;
- no privileged/device/host namespace/Docker socket/writable bind;
- only the workspace internal network on the sandbox;
- no raw secret values in environment, labels, mounts, command, image history, or inspect output;
- image reference contains the expected digest.

- [ ] **Step 4: Run adversarial probes inside the sandbox**

Using a fixed probe binary included in the fixture image rather than an interactive shell, assert:

- root filesystem write fails;
- setuid/capability escalation fails;
- mount/unshare/ptrace/raw socket fails;
- PID fork pressure stops at the configured limit;
- writes past each tmpfs quota fail;
- direct connections to `1.1.1.1:443`, RFC1918, `169.254.169.254`, host gateway, and an unlisted DNS host fail;
- proxy request to the exact fake Multica origin succeeds;
- proxy request to an unknown host, wrong port, and private target fails;
- the sandbox cannot see another workspace network created by the test.

- [ ] **Step 5: Prove rollback and cleanup**

Inject failures after network creation, after proxy creation, and after sandbox creation. Each case must leave no matching container, network, or enrollment secret. A successful delete must produce the same empty result.

- [ ] **Step 6: Run Linux acceptance twice**

Run:

```bash
cd server && AURORA_RUN_DOCKER_SECURITY_TEST=1 go test -tags=auroradocker ./internal/aurorafleet -run TestDockerSandboxLinuxSecurityBoundary -count=2 -v
```

Expected: two passes and zero labeled resources after the command.

- [ ] **Step 7: Commit Linux acceptance**

```bash
git add server/internal/aurorafleet/docker_integration_test.go \
  server/internal/aurorafleet/docker.go server/internal/auroraegress/proxy_test.go \
  deploy/aurora-sandbox/docker-security-test.sh
git commit -m "test(aurora): verify Linux sandbox isolation"
```

### Task 7: Add a macOS Docker Desktop Development Smoke and Final Verification

**Files:**
- Create: `deploy/aurora-sandbox/docker-smoke.sh`
- Modify: `.env.example`
- Modify: `docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md` only to tick Plan B after Linux evidence exists

**Interfaces:**
- Consumes: All plan B components and the fixture image; child plan D later substitutes the release image.
- Produces: Repeatable functional smoke that explicitly does not claim Linux security acceptance.

- [ ] **Step 1: Write the smoke script as a condition-driven flow**

The script must:

1. require Docker Desktop and the fixture image digest;
2. generate a temporary control token/enrollment secret under mode `0400`;
3. start fake Multica control and fake provider endpoints;
4. start fleet and egress proxy;
5. ensure one workspace node;
6. wait for enrolled/healthy state through API polling with a 90-second deadline;
7. verify a second ensure returns the same node ID;
8. delete the node and assert no labeled resources or secret files remain;
9. print `FUNCTIONAL SMOKE ONLY: AppArmor and Linux cgroup acceptance not evaluated on Docker Desktop`.

- [ ] **Step 2: Run the macOS smoke**

Run:

```bash
deploy/aurora-sandbox/docker-smoke.sh
```

Expected: exit 0 with the functional-only disclaimer. This result must not tick Task 6.

- [ ] **Step 3: Run focused Go verification**

Run:

```bash
source .env.worktree
cd server && go test ./internal/aurorafleet ./internal/auroraegress ./internal/aurora ./internal/handler ./cmd/server -count=1
```

Expected: all selected packages pass without external network calls.

- [ ] **Step 4: Run repository backend checks**

Run from repository root:

```bash
make test
git diff --check
```

Expected: both exit 0. Report any known unrelated failure with its exact rerun result instead of claiming success.

- [ ] **Step 5: Document configuration and platform boundary**

Add the exact variables from the master plan, explain digest-only images and token files, and state that Linux Task 6 is mandatory for issue #29 completion.

- [ ] **Step 6: Commit smoke and docs**

```bash
git add deploy/aurora-sandbox/docker-smoke.sh .env.example \
  docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md
git commit -m "docs(aurora): add sandbox fleet smoke"
```

## Plan B Completion Evidence

Preserve:

- focused Go test output;
- Linux Docker inspect JSON with secrets redacted;
- two consecutive Linux security-test passes;
- an empty labeled-resource listing after rollback/delete;
- macOS smoke output with its functional-only disclaimer;
- full backend test output and explicit skipped gated tests;
- confirmation that no real agent/provider call ran.
