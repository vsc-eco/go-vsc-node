# syntax=docker/dockerfile:1

# Use the official Go image as the base image
FROM golang:1.25.10 AS build

# Wasmedge Install Dependencies
RUN apt update && apt install -y git python3

# uid/gid 1000 matches the `app` user; the cache mounts below are created with
# this ownership so the non-root build can write to them.
RUN useradd -m --uid 1000 app

# Keep Go's module cache and build cache out of the image layers, and share the
# module cache between the linux/amd64 and linux/arm64 builds. Without cache
# mounts, each platform commits its own multi-GB GOMODCACHE and GOCACHE to a
# layer, which exhausts the Docker VM disk during multi-arch builds
# ("no space left on device"). The module cache is architecture-independent.
ENV GOPATH=/home/app/go \
    GOMODCACHE=/home/app/go/pkg/mod \
    GOCACHE=/home/app/.cache/go-build

USER app

# Set the working directory inside the container
WORKDIR /home/app/app

# Install Wasmedge: download install.sh pinned to the immutable release tag
# (not the mutable master branch), verify its SHA-256, then execute it.
RUN curl -sSfL https://raw.githubusercontent.com/WasmEdge/WasmEdge/0.13.4/utils/install.sh -o /tmp/wasmedge-install.sh && \
    echo "89460d9ea15f097e2831c099ee8adb6975b9ffff8a919b33874a655cd54420c0  /tmp/wasmedge-install.sh" | sha256sum -c - && \
    bash /tmp/wasmedge-install.sh -v 0.13.4 && \
    rm -f /tmp/wasmedge-install.sh

# Copy the Go module files
COPY go.mod go.sum ./

# Download the dependencies
RUN --mount=type=cache,target=/home/app/go/pkg/mod,uid=1000,gid=1000 \
    --mount=type=cache,target=/home/app/.cache/go-build,uid=1000,gid=1000 \
    go mod download

# Copy the rest of the application code
COPY --chown=app:app . .

# Generate GraphQL code and build the application
RUN --mount=type=cache,target=/home/app/go/pkg/mod,uid=1000,gid=1000 \
    --mount=type=cache,target=/home/app/.cache/go-build,uid=1000,gid=1000 \
    . /home/app/.wasmedge/env && go run github.com/99designs/gqlgen generate && go build -buildvcs=false -ldflags "-X vsc-node/modules/announcements.GitCommit=$(git rev-parse HEAD 2>/dev/null || echo unknown)" -o vsc-node vsc-node/cmd/vsc-node



FROM rockylinux:9.3-minimal AS vsc-node

RUN useradd -m app

USER app

# Set the working directory inside the container
WORKDIR /home/app/app

# Copy the binary from the build stage
COPY --from=build /home/app/app/vsc-node .

# Copy the WasmEdge from the build stage
COPY --from=build /home/app/.wasmedge /home/app/.wasmedge

# Set environment variables for WasmEdge. The entrypoint sources
# /home/app/.wasmedge/env as well, but setting these here keeps the image
# self-contained for `docker run --entrypoint ...` use and avoids referencing an
# undefined LD_LIBRARY_PATH at build time.
ENV LD_LIBRARY_PATH=/home/app/.wasmedge/lib
ENV PATH=/home/app/.wasmedge/bin:$PATH

# Create entrypoint script using sh instead of bash
RUN printf '#!/bin/sh\n\
. /home/app/.wasmedge/env\n\
exec "$@"\n' > /home/app/app/entrypoint.sh && \
chmod +x /home/app/app/entrypoint.sh

# Command to run when the container starts
ENTRYPOINT ["/home/app/app/entrypoint.sh", "./vsc-node"]
