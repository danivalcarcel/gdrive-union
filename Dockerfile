# Builds gdunion and packages it with fuse3 so it can mount inside the
# container itself. See docker-compose.yml and README's "Try it with
# Docker" section for how to run this (it needs --cap-add SYS_ADMIN,
# --device /dev/fuse, and usually --security-opt apparmor:unconfined -
# plain `docker run` without those will fail to mount).
#
# No GOARCH is set on purpose: `go build` defaults to the build stage's own
# architecture, which already matches whatever platform `docker build`
# targets (amd64, arm64, ...), so this Dockerfile doesn't need per-arch
# branches.

FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gdunion ./cmd/gdunion

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        fuse3 \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/gdunion /usr/local/bin/gdunion
RUN mkdir -p /mnt/gdrive
ENTRYPOINT ["gdunion"]
CMD ["mount", "/mnt/gdrive"]
