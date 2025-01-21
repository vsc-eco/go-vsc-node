# syntax=docker/dockerfile:1

# Use the official Go image as the base image
FROM golang:1.23

# Set the working directory inside the container
WORKDIR /app

# Copy the Go module files
COPY go.mod go.sum ./

# Download the dependencies
RUN go mod download

# Copy the rest of the application code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o main cmd/vsc-node/main.go

# Expose the port your application listens on
EXPOSE 8080

# Command to run when the container starts
CMD ["./main"]