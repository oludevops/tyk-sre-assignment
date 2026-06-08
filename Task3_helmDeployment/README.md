# Task 3 — Helm Deployment

As an application developer I want to be able to deploy this application into a Kubernetes cluster using Helm

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

**How to find the correct SHA tag:**

Option 1 — From the GitHub Actions run:
1. Go to `https://github.com/oludevops/tyk-sre-assignment/actions`
2. Click the latest successful **Build and Push Docker Image** run
3. Click **Build and push Docker image** step
4. Look for a line like `sha-655ee23` in the output — that is your tag

Option 2 — From the terminal:
```bash
# Get the short SHA of the latest commit
git log --oneline -1
# Example output: 655ee23 fix: use rest.InClusterConfig directly

# The image tag is sha- prefixed:
# ghcr.io/oludevops/sre-tool:sha-655ee23
```

Option 3 — From the container registry:
```
https://github.com/oludevops?tab=packages
```
Click `sre-tool` → all available tags are listed there.

---

## Deployment — Step by Step

### Prerequisites

- Helm v3 installed (`helm version`)
- A running Kubernetes cluster (`kubectl get nodes`)
- `kubectl` configured to point at your cluster

### Step 1 — Docker image

The image is built and pushed automatically by GitHub Actions on every push to `main`. It is already available at `ghcr.io/oludevops/sre-tool:latest` and is public — no registry login or manual build needed.

To verify the image is available:

```bash
docker pull ghcr.io/oludevops/sre-tool:latest
```

### Step 2 — Install or upgrade the Helm chart

> All `helm` commands must be run from the repo root (`~/tyk-sre-assignment`).

```bash
# Check if already installed
helm list
```

Run **one** of the following depending on the situation:

```bash
# OPTION 1 — Single cluster, first time install
helm install sre-tool ./helm/sre-tool --set clusterName=<your-cluster-name>

# OPTION 2 — Single cluster, already installed — upgrade instead
helm upgrade sre-tool ./helm/sre-tool --set clusterName=<your-cluster-name>

# OPTION 3 — Multiple clusters — deploy one instance per cluster
# Each instance monitors only the cluster it is deployed in.
# Adding a new cluster requires one helm install command — no code changes needed.
helm install sre-tool ./helm/sre-tool --set clusterName=clusterA --kube-context clusterA
helm install sre-tool ./helm/sre-tool --set clusterName=clusterB --kube-context clusterB

# Adding a new cluster (e.g. clusterC) — three steps:
#
# Step 1 — Ensure kubectl can reach the new cluster
kubectl config get-contexts
# clusterC should appear in the list. If not, add it:
# aws eks update-kubeconfig --region <region> --name clusterC   # for EKS
# minikube start --profile clusterC                             # for minikube
#
# Step 2 — Deploy the tool to the new cluster
helm install sre-tool ./helm/sre-tool --set clusterName=clusterC --kube-context clusterC
#
# Step 3 — Verify the pod is running and get the URL
kubectl get pods -l app.kubernetes.io/name=sre-tool --context=clusterC
minikube service sre-tool --url --profile clusterC   # minikube
# kubectl get svc sre-tool --context=clusterC        # EKS — wait for EXTERNAL-IP

# OPTION 4 — First time install pinned to a specific image version
# The SHA comes from: https://github.com/oludevops/tyk-sre-assignment/actions
# Click the latest successful build → look for the sha-xxxxxxx tag in the output.
# Or get it from the terminal: git log --oneline -1
helm install sre-tool ./helm/sre-tool --set clusterName=clusterA --set image.tag=sha-abc1234 --kube-context clusterA

# OPTION 5 — Install into a dedicated namespace
kubectl create namespace <your-namespace> --context=<your-context>
helm install sre-tool ./helm/sre-tool \
  --set clusterName=<your-cluster-name> \
  --kube-context <your-context> \
  --namespace <your-namespace>
```

### Step 3 — Verify the deployment

```bash
# Single cluster
kubectl get pods -l app.kubernetes.io/name=sre-tool
kubectl get svc sre-tool
kubectl get clusterrole sre-tool-role
kubectl get clusterrolebinding sre-tool-rolebinding
```

Multi-cluster — check each cluster separately:

```bash
kubectl get pods -l app.kubernetes.io/name=sre-tool --context=clusterA
```

```
NAME                       READY   STATUS    RESTARTS   AGE
sre-tool-54c489599-lsmrp   1/1     Running   0          3h15m
```

```bash
kubectl get pods -l app.kubernetes.io/name=sre-tool --context=clusterB
```

```
NAME                        READY   STATUS    RESTARTS   AGE
sre-tool-845594b798-46xmn   1/1     Running   0          3h16m
```

```bash
# Check logs to confirm in-cluster auth and cluster name
kubectl logs -l app.kubernetes.io/name=sre-tool --context=clusterA
# Expected:
# Running inside cluster — using in-cluster service account token.
# Connected to Kubernetes v1.x.x (clusterA)
# Server listening on :8080

kubectl logs -l app.kubernetes.io/name=sre-tool --context=clusterB
# Expected:
# Running inside cluster — using in-cluster service account token.
# Connected to Kubernetes v1.x.x (clusterB)
# Server listening on :8080
```

### Step 4 — Access the endpoints

**On minikube — multiple clusters:**

```bash
# Get the URLs first
minikube service sre-tool --url --profile clusterA
minikube service sre-tool --url --profile clusterB
```

**clusterA:**

```bash
curl -s http://192.*.*.*:31842/deployments/health | jq .
curl -s http://192.*.*.*:31842/healthz | jq .
```

```
# Browser
http://192.*.*.*:31842/deployments/health?format=html
http://192.*.*.*:31842/deployments/health?format=table
http://192.*.*.*:31842/healthz?format=html
```

**clusterB:**

```bash
curl -s http://192.*.*.*:31298/deployments/health | jq .
curl -s http://192.*.*.*:31298/healthz | jq .
```

```
# Browser
http://192.*.*.*:31298/deployments/health?format=html
http://192.*.*.*:31298/deployments/health?format=table
http://192.*.*.*:31298/healthz?format=html
```

**On AWS EKS:**

```bash
# Install with default LoadBalancer — one command per cluster
helm install sre-tool ./helm/sre-tool --set clusterName=clusterA --kube-context clusterA
helm install sre-tool ./helm/sre-tool --set clusterName=clusterB --kube-context clusterB

# Get the load balancer DNS name for each cluster (may take 1-2 min to provision)
kubectl get svc sre-tool --context=clusterA
kubectl get svc sre-tool --context=clusterB

curl -s http://<clusterA-EXTERNAL-IP>:8080/deployments/health | jq .
curl -s http://<clusterB-EXTERNAL-IP>:8080/deployments/health | jq .

# Browser — accessible from any machine including Windows
http://<clusterA-EXTERNAL-IP>:8080/deployments/health?format=html
http://<clusterB-EXTERNAL-IP>:8080/deployments/health?format=html
```

### Upgrading

```bash
# Multi-cluster — upgrade each independently
helm upgrade sre-tool ./helm/sre-tool --set clusterName=clusterA --kube-context clusterA
helm upgrade sre-tool ./helm/sre-tool --set clusterName=clusterB --kube-context clusterB
```

### Uninstalling

```bash
# Multi-cluster
helm uninstall sre-tool --kube-context clusterA
helm uninstall sre-tool --kube-context clusterB
```

---

## Implementation Notes

- **In-cluster vs out-of-cluster:** When `--kubeconfig` is not provided, `client-go` automatically detects it is running inside the cluster and reads the service account token from the mounted path. The same binary works both locally (with `--kubeconfig`) and inside the cluster (without it).
- **Cluster name identification:** The `clusterName` value is passed to the pod as a `CLUSTER_NAME` environment variable. Every JSON response and HTML dashboard title includes this name so the operator always knows which cluster the data is for.
- **Multi-cluster pattern:** Deploy one instance of the tool per cluster using `--set clusterName=<name> --kube-context <context>`. Each instance monitors only its own cluster using the in-cluster service account token. Adding a new cluster requires one `helm install` command — no code changes needed.
- **Static binary:** `CGO_ENABLED=0` produces a binary with no C library dependencies. This is required for the binary to run in a minimal Alpine image.
- **Non-root container:** The Dockerfile creates a dedicated `sreuser` with numeric UID 1001. The Helm chart enforces `runAsNonRoot: true` and `runAsUser: 1001` — Kubernetes requires a numeric UID to verify the container is not running as root.
- **Liveness vs readiness probes:** Both hit `/healthz`. The liveness probe restarts the pod if the API server becomes unreachable. The readiness probe removes the pod from Service endpoints until it confirms connectivity.
- **Layer caching:** Dependencies are downloaded in a separate Docker layer before source code is copied. Rebuilds after code-only changes skip the `go mod download` step entirely, making continuous integration significantly faster.
