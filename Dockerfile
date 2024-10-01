# ------------ Build Stage ------------ #
FROM golang:1.23-alpine AS builder

# Use BuildKit's automatic build arguments for platform
ARG TARGETOS
ARG TARGETARCH

# Set the environment variables for the target OS and architecture
ENV GOOS=$TARGETOS
ENV GOARCH=$TARGETARCH
ENV CGO_ENABLED=0

# Set the working directory inside the container
WORKDIR /app

# Copy go.mod and go.sum files to download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code into the container
COPY . .

# Build the Go application
RUN go build -o websocket-proxy .

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
