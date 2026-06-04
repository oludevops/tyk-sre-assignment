# =============================================================================
# Stage 1 — Builder
# =============================================================================
# We use the official Go image based on Alpine Linux.
# Alpine is a minimal Linux distribution (~5MB) which keeps the builder lean.
# This entire stage is a temporary workbench — it never ships to production.
FROM golang:1.22-alpine AS builder

# Set the working directory inside the container.
# All subsequent COPY and RUN commands happen relative to /app.
# Docker creates this directory automatically if it does not exist.
WORKDIR /app

# Copy the dependency manifest files first — before any source code.
# This is a Docker layer-caching optimisation:
#   - Docker builds images in layers, one per instruction.
#   - Each layer is cached. If nothing changed in that layer, Docker reuses
#     the cache instead of re-running the command.
#   - By copying go.mod and go.sum separately, we ensure that dependency
#     downloads are only re-run when the dependency files themselves change.
#   - Changing main.go (which happens often) will NOT bust the module cache.
COPY go.mod go.sum ./

# Download all third-party dependencies declared in go.mod.
# Examples: k8s.io/client-go, github.com/stretchr/testify, etc.
# These are cached in a Docker layer — subsequent builds skip this step
# unless go.mod or go.sum changes.
RUN go mod download

# Copy the rest of the source code into the container.
# We use COPY . . (everything) rather than listing files individually so that
# any new .go files added in future are picked up automatically.
# Note: go build ignores *_test.go files, so test files ride along harmlessly.
COPY . .

# Compile the Go source into a static binary named sre-tool.
#
#   CGO_ENABLED=0  — disables C bindings. Produces a fully static binary with
#                    no dependency on system C libraries (libc, etc.). This is
#                    required for the binary to run in a minimal Alpine image.
#
#   GOOS=linux     — targets Linux explicitly. Important when building on a
#                    Mac or Windows machine where the host OS differs.
#
#   -o sre-tool    — names the output binary. Without -o, Go names it after
#                    the module or directory.
#
#   .              — compile the package in the current directory (/app).
RUN CGO_ENABLED=0 GOOS=linux go build -o sre-tool .

# =============================================================================
# Stage 2 — Runtime
# =============================================================================
# A fresh, minimal Alpine image — approximately 7MB.
# Nothing from Stage 1 (compiler, source code, modules) is included here.
# Only what we explicitly copy with COPY --from=builder arrives in this stage.
FROM alpine:3.19

# Install CA certificates so the binary can make TLS connections.
# The Kubernetes API server uses HTTPS — without CA certs the TLS handshake
# would fail with a certificate verification error.
RUN apk --no-cache add ca-certificates

# Create a non-root user to run the binary.
# Running as root inside a container is a security risk — if an attacker
# escapes the container, they land as root on the host. A dedicated user
# limits the blast radius.
RUN addgroup -S sregroup && adduser -S sreuser -G sregroup

# Copy only the compiled binary from Stage 1 into this minimal image.
# --from=builder references the first stage by its AS name.
# Everything else from Stage 1 stays behind and is discarded.
COPY --from=builder /app/sre-tool /usr/local/bin/sre-tool

# Switch to the non-root user for all subsequent instructions and at runtime.
USER sreuser

# Document that the container listens on port 8080.
# EXPOSE is informational only — it does not actually publish the port.
# Kubernetes uses this as a hint when configuring Services.
EXPOSE 8080

# The command that runs when Kubernetes starts the container.
# We use ENTRYPOINT (not CMD) so the binary always runs and cannot be
# accidentally overridden by a kubectl run command.
# The tool detects it is running inside the cluster automatically and uses
# the pod's service account token — no --kubeconfig flag needed.
ENTRYPOINT ["sre-tool"]
