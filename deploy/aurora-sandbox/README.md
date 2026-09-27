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
the four provider secrets are host files mounted read-only at
`/run/secrets/anthropic-api-key`, `/run/secrets/ark-api-key`,
`/run/secrets/openai-api-key`, and `/run/secrets/volc-asr-api-key`.

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
`mode=max` and SBOM attestations. This macOS host has not produced a complete
image yet: `pnpm fetch --filter` still pulls the whole workspace lockfile
(follow-up #149), and the base retains `apt`/`apt-get`/`dpkg` in the final
stage (follow-up #150, which the image verifier flags).

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

## Local fake smoke and Linux acceptance

Run the Linux acceptance matrix on a Docker Engine host with AppArmor for the
full isolation/egress boundary plus the fake-provider pipelines:

```bash
AURORA_RUN_DOCKER_SECURITY_TEST=1 deploy/aurora-sandbox/docker-security-test.sh
```

On macOS, use the functional smoke instead. It prints
`FUNCTIONAL SMOKE ONLY` and explicitly records that AppArmor/cgroup security
acceptance was not evaluated.

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

## Known follow-ups

- #149 — `pnpm fetch` ignores `--filter` and pulls the whole workspace
  lockfile, so the image build has not completed on this macOS host.
- #150 — the base image retains `apt`/`apt-get`/`dpkg`; removing them is a
  separate reviewed change.
- #140 — the Seedance license gate remains an open distribution blocker; only
  the vendored Seedream tree ships today.
