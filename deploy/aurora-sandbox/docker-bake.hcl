# Buildx Bake targets for the Aurora managed sandbox and egress proxy images.
#
# Usage from the repository root (the context paths below resolve against the
# invocation directory):
#   docker buildx bake -f deploy/aurora-sandbox/docker-bake.hcl sandbox egress --load
#
# CI sets CACHE_REF (and pushes), REVISION, TAG, and SOURCE_DATE_EPOCH.

variable "REGISTRY" {
  default = "ghcr.io/eanfs"
}

variable "TAG" {
  default = "dev"
}

variable "REVISION" {
  default = "unknown"
}

variable "SOURCE_DATE_EPOCH" {
  default = "0"
}

# Empty disables registry cache, which keeps a local --load build offline.
variable "CACHE_REF" {
  default = ""
}

group "default" {
  targets = ["sandbox", "egress"]
}

target "common" {
  context = "deploy/aurora-sandbox"
  contexts = {
    server   = "server"
    reporoot = "."
    scripts  = "scripts"
  }
  platforms = ["linux/amd64", "linux/arm64"]
  args = {
    SOURCE_DATE_EPOCH = SOURCE_DATE_EPOCH
  }
  labels = {
    "org.opencontainers.image.source"   = "https://github.com/eanfs/multica"
    "org.opencontainers.image.revision" = REVISION
    "org.opencontainers.image.licenses" = "AGPL-3.0-or-later"
    "org.opencontainers.image.title"    = "multica-aurora"
    "org.opencontainers.image.created"  = "1970-01-01T00:00:00Z"
  }
  attest = [
    "type=provenance,mode=max",
    "type=sbom",
  ]
  cache-from = CACHE_REF == "" ? [] : ["type=registry,ref=${CACHE_REF},mode=max"]
  cache-to   = CACHE_REF == "" ? [] : ["type=registry,ref=${CACHE_REF},mode=max"]
}

target "sandbox" {
  inherits   = ["common"]
  dockerfile = "Dockerfile"
  tags       = ["${REGISTRY}/multica-aurora-sandbox:${TAG}"]
}

target "egress" {
  inherits   = ["common"]
  dockerfile = "Dockerfile.egress"
  tags       = ["${REGISTRY}/multica-aurora-egress:${TAG}"]
}
