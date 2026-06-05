# Task 3 — Helm Deployment

**As an application developer I want to be able to deploy this application into a Kubernetes cluster using Helm**
---
## What Was Built

| Artifact | Location | Purpose |
|----------|----------|---------|
| `Dockerfile` | `golang/Dockerfile` | Multi-stage build — compiles the Go binary and packages it into a minimal Alpine image |
| Helm chart | `helm/sre-tool/` | Deploys the tool and all required Kubernetes resources |
| GitHub Actions workflow | `.github/workflows/docker-publish.yml` | Builds and pushes the Docker image to `ghcr.io` automatically |

---

## Dockerfile — Multi-Stage Build

The Dockerfile uses two stages so the final image contains only the compiled binary and nothing else.

### Stage 1 — Builder

Uses `golang:1.22-alpine` as a temporary workbench to compile the source code.

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o sre-tool .
```

Line by line:

- `WORKDIR /app` — sets the working directory inside the container. All subsequent commands run relative to `/app`.
- `COPY go.mod go.sum ./` — copies the dependency manifest files first, before the source code. This is a Docker layer-caching optimisation — if only `main.go` changes, Docker reuses the cached `go mod download` layer and skips re-downloading dependencies.
- `RUN go mod download` — downloads all third-party packages (`k8s.io/client-go`, `testify`, etc.) into the container.
- `COPY . .` — copies all source files. `go build` ignores `_test.go` files automatically.
- `CGO_ENABLED=0` — produces a fully static binary with no C library dependencies, required for it to run in a minimal Alpine image.
- `GOOS=linux` — targets Linux explicitly, important when building on a Mac or Windows machine.
- `-o sre-tool` — names the output binary.

### Stage 2 — Runtime

A fresh minimal `alpine:3.19` image (~7MB). Nothing from Stage 1 is included except the binary.

```dockerfile
FROM alpine:3.19
RUN apk --no-cache add ca-certificates
RUN addgroup -S -g 1001 sregroup && adduser -S -u 1001 -G sregroup sreuser
COPY --from=builder /app/sre-tool /usr/local/bin/sre-tool
USER sreuser
EXPOSE 8080
ENTRYPOINT ["sre-tool"]
```

- `ca-certificates` — required for TLS connections to the Kubernetes API server (HTTPS).
- `adduser -u 1001 sreuser` — runs the binary as a non-root user with a numeric UID. Kubernetes requires a numeric UID to enforce `runAsNonRoot`.
- `COPY --from=builder` — pulls only the compiled binary from Stage 1.
- `ENTRYPOINT` — the command Kubernetes runs when the container starts.

**Final image size: ~17MB** vs ~600MB if the Go toolchain were included.

---

## Helm Chart Structure

```
helm/sre-tool/
├── Chart.yaml                    # Chart name, version, description
├── values.yaml                   # Default configuration values
└── templates/
    ├── deployment.yaml           # Runs the container as a pod
    ├── service.yaml              # Stable network endpoint for the pod
    ├── serviceaccount.yaml       # Identity the pod runs as
    ├── clusterrole.yaml          # Permissions: list namespaces + deployments
    └── clusterrolebinding.yaml   # Binds the role to the service account
```

### Why Each Resource Is Needed

**Deployment** — Manages the pod lifecycle. If the pod crashes, the Deployment controller starts a replacement automatically. It also handles rolling updates so there is no downtime when a new image is deployed.

**Service** — Pod IP addresses change on every restart. The Service provides a stable DNS name (`sre-tool.default.svc.cluster.local`) that always routes to the current healthy pod.

**ServiceAccount** — The identity the pod runs as. Kubernetes automatically mounts a token for this account into the pod at `/var/run/secrets/kubernetes.io/serviceaccount/token`. The Go `client-go` library reads this token automatically (in-cluster configuration) — no `--kubeconfig` flag needed inside the cluster.

**ClusterRole** — Grants the minimum permissions the tool needs:
- `namespaces: list` — to iterate all namespaces when checking deployments
- `deployments: list` — to read Deployment specs and status

**ClusterRoleBinding** — Connects the ClusterRole to the ServiceAccount. Without this, the ServiceAccount exists but has no permissions.

### RBAC — Principle of Least Privilege

```yaml
rules:
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["list"]
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["list"]
```

The `/healthz` endpoint calls `Discovery().ServerVersion()` which hits `/version` on the API server. That endpoint is accessible to any authenticated principal and requires no explicit RBAC rule.

---

## GitHub Actions — Automated Image Build

The workflow at `.github/workflows/docker-publish.yml` triggers on every push to `main` that changes files under `golang/`. It:

1. Checks out the code
2. Logs in to `ghcr.io` using the automatic `GITHUB_TOKEN` — no secrets to configure manually
3. Builds the Docker image from `golang/Dockerfile`
4. Pushes two tags:
   - `ghcr.io/oludevops/sre-tool:latest` — always points to the most recent build
   - `ghcr.io/oludevops/sre-tool:sha-<short-git-sha>` — immutable tag tied to the exact commit

The SHA tag is what you pass to `helm upgrade --set image.tag=sha-abc1234` so you always know exactly which commit is running in the cluster.

---

## Deployment — Step by Step

### Prerequisites

- Docker installed and authenticated to `ghcr.io`
- Helm v3 installed (`helm version`)
- A running Kubernetes cluster (`kubectl get nodes`)
- `kubectl` configured to point at your cluster

### Step 1 — Build and push the Docker image

GitHub Actions handles this automatically on every push to `main`. To build and push manually:

```bash
# Log in to the GitHub Container Registry
echo $GITHUB_TOKEN | docker login ghcr.io -u oludevops --password-stdin

# Build the image — run from the repo root
docker build -t ghcr.io/oludevops/sre-tool:latest golang/

# Push to the registry
docker push ghcr.io/oludevops/sre-tool:latest
```

### Step 2 — Make the image accessible to your cluster

**For minikube (local testing):**

```bash
minikube image load ghcr.io/oludevops/sre-tool:latest
```

Then update `values.yaml` to set `imagePullPolicy: Never` so Kubernetes uses the locally loaded image.

**For a real cluster using ghcr.io:**

Make the package public on GitHub: Profile → Packages → sre-tool → Package settings → Change visibility → Public.

### Step 3 — Install or upgrade the Helm chart

> All `helm` commands must be run from the repo root (`~/tyk-sre-assignment`).

```bash
# Check if already installed
helm list

# First time install
helm install sre-tool ./helm/sre-tool

# Already installed — upgrade instead
helm upgrade sre-tool ./helm/sre-tool

# Override the image tag with a specific git SHA
helm install sre-tool ./helm/sre-tool --set image.tag=sha-abc1234

# Install into a dedicated namespace
kubectl create namespace sre
helm install sre-tool ./helm/sre-tool --namespace sre
```

### Step 4 — Verify the deployment

```bash
kubectl get pods -l app.kubernetes.io/name=sre-tool
kubectl get svc sre-tool
kubectl get clusterrole sre-tool-role
kubectl get clusterrolebinding sre-tool-rolebinding

# Check logs to confirm in-cluster auth worked
kubectl logs -l app.kubernetes.io/name=sre-tool
# Expected:
# Connected to Kubernetes v1.x.x
# Server listening on :8080
```

### Step 5 — Access the endpoints

**On minikube:**

```bash
# Get the URL
minikube service sre-tool --url
# Prints: http://192.168.49.2:30318

curl -s http://192.168.49.2:30318/healthz | jq .
curl -s http://192.168.49.2:30318/deployments/health | jq .
```

**On AWS EKS:**

```bash
# Get the load balancer DNS name
kubectl get svc sre-tool
# EXTERNAL-IP: a1b2c3d4.eu-west-1.elb.amazonaws.com (takes 1-2 min to provision)

curl -s http://<EXTERNAL-IP>:8080/healthz | jq .
curl -s http://<EXTERNAL-IP>:8080/deployments/health | jq .
```

### Upgrading

```bash
helm upgrade sre-tool ./helm/sre-tool --set image.tag=sha-newsha
```

### Uninstalling

```bash
helm uninstall sre-tool
```

---

## Implementation Notes

- **In-cluster vs out-of-cluster:** When `--kubeconfig` is not provided, `client-go` automatically detects it is running inside the cluster and reads the service account token from the mounted path. The same binary works both locally (with `--kubeconfig`) and inside the cluster (without it).
- **Static binary:** `CGO_ENABLED=0` produces a binary with no C library dependencies. This is required for the binary to run in a minimal Alpine image.
- **Non-root container:** The Dockerfile creates a dedicated `sreuser` with numeric UID 1001. The Helm chart enforces `runAsNonRoot: true` and `runAsUser: 1001` — Kubernetes requires a numeric UID to verify the container is not running as root.
- **Liveness vs readiness probes:** Both hit `/healthz`. The liveness probe restarts the pod if the API server becomes unreachable. The readiness probe removes the pod from Service endpoints until it confirms connectivity.
- **Layer caching:** Dependencies are downloaded in a separate Docker layer before source code is copied. Rebuilds after code-only changes skip the `go mod download` step entirely, making CI significantly faster.
