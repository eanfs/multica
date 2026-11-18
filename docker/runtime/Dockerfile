# Node runtime image: the Multica CLI built from this checkout plus a pinned
# Claude Code, running as the fixed non-root node identity.
FROM golang:1.27.3-alpine AS cli

RUN apk add --no-cache git

WORKDIR /src

COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download
COPY server/ ./server/
RUN cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/multica ./cmd/multica

FROM node:22-bookworm-slim

ARG CLAUDE_CODE_VERSION=2.1.0

RUN apt-get update \
    && apt-get install -y --no-install-recommends git bash ca-certificates curl tini \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && npm cache clean --force

# The node identity is fixed at 1000:1000 because Fleet creates the container
# with User=1000:1000 and seeds the volumes for that identity.
# node:22-bookworm-slim already ships a group/user at 1000:1000, so this image
# reuses that identity instead of creating a conflicting one. Every path under
# the data volume mount must already belong to it: Docker copies image content
# into an empty named volume with its original ownership, so a root-owned
# workspaces/ would leave the node unable to write.
RUN mkdir -p /home/multica/workspaces /run/secrets/runtime \
    && chown -R 1000:1000 /home/multica \
    && chmod 0755 /home/multica /home/multica/workspaces

COPY --from=cli /out/multica /usr/local/bin/multica
COPY docker/runtime/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh

ENV HOME=/home/multica
WORKDIR /home/multica
USER 1000:1000

# Fleet observes this health status through the Docker API; the daemon port is
# never published to the host.
HEALTHCHECK --interval=10s --timeout=5s --start-period=120s --retries=3 \
    CMD curl -fsS http://127.0.0.1:19514/health || exit 1

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
