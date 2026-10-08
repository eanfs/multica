# Aurora managed sandbox images

This directory builds, locks, scans, signs, and documents the two release
images for the Aurora managed execution sandbox:

- `ghcr.io/eanfs/multica-aurora-sandbox` — the managed daemon runtime, the
  MCP broker, the reviewed Node tool surface, and the patched vendor tree;
- `ghcr.io/eanfs/multica-aurora-egress` — the minimal egress proxy.

Both are multi-architecture OCI indexes published for `linux/amd64` and
`linux/arm64`, signed keylessly, and shipped with SPDX SBOMs, SLSA
provenance, and Trivy scan reports.

## Platform boundary

Linux Docker Engine is the release target and the only environment that
evaluates the security boundary (AppArmor, seccomp, cgroups, and the kernel
gates). Docker Desktop on macOS is a functional developer smoke only: it can
run the fake-provider pipelines but it cannot assert the Linux isolation
guarantees. The managed image never runs with provider credentials baked in;
the three provider secrets are host files mounted read-only at
`/run/secrets/anthropic-api-key`, `/run/secrets/ark-api-key`,
and `/run/secrets/volc-asr-api-key`.

## Locked inputs

| Input                        | Source                                                   |
| ---------------------------- | -------------------------------------------------------- |
| Base images and Go toolchain | `versions.json` (digest-pinned)                          |
| Debian packages              | `apt-packages.lock` (dated snapshot, both architectures) |
| Node runtime packages        | `runtime/package.json` and the root `pnpm-lock.yaml`     |
| Vendor source and patches    | `vendor/volcengine/vendor-lock.json`                     |

`node scripts/verify-aurora-sandbox-locks.mjs` cross-checks the Dockerfiles,
the Debian lock, the workspace lockfile, the Go module, and the vendor locks.
`node scripts/verify-aurora-sandbox-locks.mjs --workflow` additionally enforces
the CI supply-chain policy (commit-SHA-pinned actions, least-privilege
permissions, digest-only image references, no secret in build args, and the
required scan/SBOM/provenance/sign steps).

## Building locally

The image-local context is this directory, and the Go module, repository root,
and script trees arrive as Bake named contexts:

```bash
docker buildx bake -f deploy/aurora-sandbox/docker-bake.hcl sandbox egress --load \
  --set '*.platform=linux/amd64'
```

CI builds each image independently with the same named contexts
(`server=server`, `reporoot=.`, `scripts=scripts`) and the same
`.dockerignore`. Pull requests build the host platform with `push: false`;
the publish job builds `linux/amd64,linux/arm64` with BuildKit provenance
`mode=max` and SBOM attestations. The runtime install is filtered to the
broker package graph; the final stage drops the apt/dpkg/perl frontends, the
Node package-manager shims (npm/npx/corepack/pnpm/yarn), and their caches; and
the verifier requires the real platform-native Claude Code launcher rather than
the npm install stub.

## Verifying an image

```bash
scripts/verify-aurora-sandbox-image.sh \
  ghcr.io/eanfs/multica-aurora-sandbox@sha256:<index-digest> \
  ghcr.io/eanfs/multica-aurora-egress@sha256:<index-digest>
```

The verifier asserts the configured user, entrypoint, health check, and
architecture; the per-architecture size budget; the required binaries and
locked versions; the patched vendor-tree hash; the absence of forbidden
package-manager/download/SSH/Git binaries; writable root-owned runtime
directories; and any token/key pattern in files, config, labels, environment,
or `docker history --no-trunc`.

## Running the acceptance modes

Both scripts require image references that resolve to an immutable digest or
content-addressed image ID. A caller may pass `sha256:<64 lowercase hex>`, a
`<name>@sha256:<64 hex>` reference, or a tag already present in the local image
store; the script resolves a local tag to its repository digest or
content-addressed image ID and never pulls. A missing local image is an error.

Each run writes a sanitized JSON report under
`.scratch/aurora-sandbox-acceptance/` (override with
`AURORA_ACCEPTANCE_REPORT_DIR`). A report carries the host platform, the Docker
client and server versions, kernel, architecture, cgroup mode, AppArmor status,
the resolved image digests, every test name and duration, pass/fail/skip counts,
and the overall result. Reports contain no tokens, provider URLs, or prompts.
The directory is gitignored; CI uploads its copies as workflow artifacts.

### Linux security acceptance

Run on a Linux Docker Engine host with AppArmor. The script builds the fixture
probe and egress images unless `AURORA_FIXTURE_SANDBOX_REF` and
`AURORA_FIXTURE_PROXY_REF` name a prebuilt digest-pinned pair, loads the
AppArmor profile, records the inventory, and runs the isolation/egress boundary
plus the containerized fake pipeline smoke inside the release sandbox image:

```bash
AURORA_PIPELINE_IMAGE='ghcr.io/eanfs/multica-aurora-sandbox@sha256:<index-digest>' \
  deploy/aurora-sandbox/docker-security-test.sh
```

`AURORA_PIPELINE_IMAGE` names the release sandbox image that carries the Node,
Chromium, and FFmpeg runtime; every container run uses `--pull never`. The
report is `.scratch/aurora-sandbox-acceptance/linux-acceptance.json`. CI
resolves the locally built sandbox image to its content-addressed image ID
before invoking the script, so the workflow passes an immutable reference even
though the image is never pushed to a registry.

### macOS Docker Desktop functional smoke

Run on macOS Docker Desktop:

```bash
AURORA_PIPELINE_IMAGE='ghcr.io/eanfs/multica-aurora-sandbox@sha256:<index-digest>' \
  deploy/aurora-sandbox/docker-smoke.sh
```

The script starts fake Multica control and provider endpoints, provisions a
workspace node through the authenticated fleet API, waits for the online/idle
state, verifies the egress boundary, runs the real release sandbox image
through the containerized fake `xhs-image` pipeline, confirms a repeat ensure is
idempotent, and deletes the node. It prints `FUNCTIONAL SMOKE ONLY` and states
that AppArmor/cgroup security acceptance was not evaluated; the report records
`security_acceptance_evaluated: false` at
`.scratch/aurora-sandbox-acceptance/macos-smoke.json`. Without
`AURORA_PIPELINE_IMAGE`, or when the sandbox image has no Node runtime, the
fake-pipeline step is recorded as `skip`, not `pass`.

## Digest-only deployment

Fleet execution always references an image by repository and immutable digest —
never by a mutable tag:

```text
ghcr.io/eanfs/multica-aurora-sandbox@sha256:<verified-index-digest>
ghcr.io/eanfs/multica-aurora-egress@sha256:<verified-index-digest>
```

Tags may aid discovery, but deployment output and this documentation print
digest references only. The publish job emits the index digests as a workflow
artifact, and the `verify-published` job fails if a pushed tag resolves to a
different digest or if either architecture is missing from the index.

## Supply-chain workflow

`.github/workflows/aurora-sandbox.yml` runs on pull requests that touch the
sandbox, runtime, or fleet files, and on pushes to `main` in
`eanfs/multica`:

1. **verify** — checkout with credentials disabled; verify locks, vendor
   patches, and licenses; run the focused Go and Node tests; build the
   host-platform sandbox and egress images without pushing; run the image
   verifier and the fake container smoke on Linux; generate SPDX JSON with
   Syft; scan the filesystem and both images with Trivy; enforce the release
   policy; and upload the reports as workflow artifacts with 30-day retention.
2. **publish** — push both architectures to GHCR, produce OCI index digests,
   BuildKit provenance `mode=max`, SPDX JSON, and GitHub artifact
   attestations; install Cosign from a commit-pinned action; sign each index
   digest keylessly; and attach SPDX and provenance attestations. The job runs
   only for `eanfs/multica` on `main` and requests exactly
   `contents: read`, `packages: write`, `id-token: write`, and
   `attestations: write`.
3. **verify-published** — after the push, run `cosign verify`,
   `cosign verify-attestation --type spdxjson`, and provenance verification
   against the exact digest and repository identity.

Every action is pinned to a full commit SHA. The human-readable release tag is
kept in a trailing comment only.

## Vulnerability release policy

The release policy fails on any secret finding and any unfixed Critical
vulnerability. A High vulnerability blocks unless
`.github/aurora-sandbox-vex.json` names the exact CVE, package and version,
image, status (`not_affected` or `fixed`), a technical justification, an
approver, an issue URL, and an expiry no more than 30 days away. The verifier
rejects expired, wildcard, or missing-field entries:

```bash
node scripts/verify-aurora-sandbox-locks.mjs --workflow
```

## Gated real provider and agent smokes

`server/pkg/agent/aurora_sandbox_smoke_test.go` holds the real provider and
agent smokes behind the `agentintegration` build tag. They are double-gated
and never run by default:

1. `MULTICA_RUN_REAL_AGENT_SMOKE=1` is checked as the first statement, before
   any executable lookup, Docker image pull, credential-file read, account
   access, or network call; and
2. each subtest checks its own opt-in before reading its credential file:
   `AURORA_RUN_CLAUDE_SMOKE`, `AURORA_RUN_SEEDREAM_SMOKE`,
   `AURORA_RUN_SEEDANCE_SMOKE`, `AURORA_RUN_VOLC_ASR_SMOKE`,
   `AURORA_RUN_HYPERFRAMES_SMOKE`, and
   `AURORA_RUN_CHROMIUM_SMOKE`.

There is no run-everything switch: a missing opt-in skips its subtest before
its secret is touched, and the default tagged run performs one immediate skip
at the global gate.

An enabled subtest drives a live Multica stack through the public Aurora HTTP
API only. It creates a fresh workspace and generation, waits conditionally for
a terminal status, verifies the artifact kind, format, SHA-256 and nonzero
size, verifies the exact credit settlement from the ledger, and then deletes
its assets and workspace. It never runs a vendor script or a provider CLI on
the host, and it uses the digest-pinned sandbox image named by
`AURORA_SANDBOX_IMAGE` (a tag-only reference is rejected).

Configuration:

| Variable | Purpose |
| --- | --- |
| `AURORA_SMOKE_BASE_URL` | Live Multica stack origin |
| `AURORA_SMOKE_API_TOKEN` | Human API token for the smoke account |
| `AURORA_SANDBOX_IMAGE` | Digest-pinned sandbox image; a tag-only reference is rejected |
| `AURORA_SMOKE_WORKSPACE_ID` | Optional existing workspace; empty creates a fresh one |
| `AURORA_SMOKE_ANTHROPIC_KEY_FILE` | Mode-0400 Anthropic credential file |
| `AURORA_SMOKE_ARK_KEY_FILE` | Mode-0400 Volcengine ARK credential file |
| `AURORA_SMOKE_VOLC_ASR_KEY_FILE` | Mode-0400 Volcengine ASR credential file |

Run one subtest directly only when authorized and when the credential and live
stack exist:

```bash
cd server && MULTICA_RUN_REAL_AGENT_SMOKE=1 \
  go test -tags=agentintegration ./pkg/agent \
  -run '^TestAuroraSandboxRealProviderSmoke$' -count=1 -v
```

`.github/workflows/aurora-sandbox.yml` exposes one `workflow_dispatch` boolean
per subtest. Each smoke job requires the protected `aurora-provider-smoke`
environment, is disabled for pull requests, materializes only the selected
secret into a mode-0400 temporary file, applies a 35-minute test deadline,
stops after one provider create per operation, and uploads a sanitized log
report. Logs contain test and provider/model names, Multica IDs, duration,
status, byte count, and credit delta only; never prompts, document text,
keys, tokens, signed URLs, provider responses, or artifact bytes.

## Known follow-ups

- #117 — the gated real agent and provider smokes are implemented and provably
  skip by default; they have not been executed and remain unproven until an
  authorized operator supplies provider credentials and a live stack. They are
  recorded as skipped, never failed or passed.
- The Seedance and Seedream vendor trees (#140) are both vendored, patched, and
  verified. The Seedance `LICENSE.upstream` is an owner-authorized
  reconstruction of the standard MIT text, recorded as such in
  `vendor/volcengine/vendor-lock.json`; it is not an upstream file.
