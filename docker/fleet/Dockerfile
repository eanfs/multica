# Fleet control-plane image. Fleet reaches the Docker Engine API through the
# socket Compose mounts and never shells out to the Docker CLI.
FROM golang:1.27.3-alpine AS build

RUN apk add --no-cache git

WORKDIR /src

COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download
COPY server/ ./server/
RUN cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/fleet ./cmd/fleet

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata curl

COPY --from=build /out/fleet /usr/local/bin/fleet

ENTRYPOINT ["/usr/local/bin/fleet"]
