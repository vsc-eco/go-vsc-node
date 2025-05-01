# syntax=docker/dockerfile:1

# Use the official Go image as the base image
FROM golang:1.24.1 AS build

# Wasmedge Install Dependencies
RUN apt install -y git python3

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

# Build the application
RUN go build -buildvcs=false -ldflags "-X vsc-node/modules/announcements.GitCommit=$(git rev-parse HEAD)" -o vsc-node vsc-node/cmd/vsc-node

# Build VM Runner
RUN . /home/app/.wasmedge/env && go build -buildvcs=false -o vm-runner vsc-node/cmd/vm-runner



FROM rockylinux:9.3-minimal

RUN useradd -m app

USER app

# Set the working directory inside the container
WORKDIR /home/app/app

# Copy the binary from the build stage
COPY --from=build /home/app/app/vsc-node .

# Copy the binary from the build stage
COPY --from=build /home/app/app/vm-runner .

# Copy the WasmEdge from the build stage
COPY --from=build /home/app/.wasmedge /home/app/.wasmedge

# Command to run when the container starts
ENTRYPOINT ["./vsc-node"]