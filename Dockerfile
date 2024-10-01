# ------------ Build Stage ------------ #
FROM --platform=$BUILDPLATFORM golang:1.21-bullseye AS builder

# Build arguments
ARG TARGETOS
ARG TARGETARCH
ARG BUILDPLATFORM

# Environment variables
ENV GOOS=$TARGETOS
ENV GOARCH=$TARGETARCH
ENV CGO_ENABLED=0
ENV GOCACHE=off
ENV GOTELEMETRY=off
ENV GOFLAGS="-buildvcs=false"

# Set the working directory inside the container
WORKDIR /app

# Copy go.mod and go.sum files to download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code into the container
COPY . .

# Build the Go application
RUN go build -ldflags="-buildid=" -buildvcs=false -o websocket-proxy .

# ------------ Run Stage ------------ #
FROM alpine:latest

# Set the working directory inside the container
WORKDIR /app

# Copy the built binary from the builder stage
COPY --from=builder /app/websocket-proxy .

# Expose the port that the application listens on
EXPOSE 8080

# Run the executable
CMD ["./websocket-proxy"]
