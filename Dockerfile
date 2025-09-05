# syntax=docker/dockerfile:1

# Use the official Go image as the base image
FROM golang:1.24.1 AS build

# Wasmedge Install Dependencies
RUN apt update && apt install -y git python3

RUN useradd -m app

USER app

# Set the working directory inside the container
WORKDIR /home/app/app

# Install Wasmedge
RUN curl -sSf https://raw.githubusercontent.com/WasmEdge/WasmEdge/master/utils/install.sh | bash -s -- -v 0.13.4

# Copy the Go module files
COPY go.mod go.sum ./

# Download the dependencies
RUN go mod download

# Copy the rest of the application code
COPY --chown=app:app . .

# Source the WasmEdge environment and build the application
RUN . /home/app/.wasmedge/env && go build -buildvcs=false -ldflags "-X vsc-node/modules/announcements.GitCommit=$(git rev-parse HEAD)" -o vsc-node vsc-node/cmd/vsc-node



FROM rockylinux:9.3-minimal

RUN useradd -m app

USER app

# Set the working directory inside the container
WORKDIR /home/app/app

# Copy the binary from the build stage
COPY --from=build /home/app/app/vsc-node .

# Copy the WasmEdge from the build stage
COPY --from=build /home/app/.wasmedge /home/app/.wasmedge

# Set environment variables for WasmEdge
ENV LD_LIBRARY_PATH=/home/app/.wasmedge/lib:$LD_LIBRARY_PATH
ENV PATH=/home/app/.wasmedge/bin:$PATH

# Create entrypoint script using sh instead of bash
RUN printf '#!/bin/sh\n\
. /home/app/.wasmedge/env\n\
exec "$@"\n' > /home/app/app/entrypoint.sh && \
chmod +x /home/app/app/entrypoint.sh

# Command to run when the container starts
ENTRYPOINT ["/home/app/app/entrypoint.sh", "./vsc-node"]
