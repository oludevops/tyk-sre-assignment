# Task 2 — API Server Health Check

**As an SRE I want to always know whether this tool can successfully communicate with the configured k8s API server.**

---

## What Was Built

The existing `/healthz` endpoint previously returned a static plain-text `ok` response — it only confirmed the process was alive, not that it could talk to Kubernetes.

It now actively probes the Kubernetes API server on every request by calling `Discovery().ServerVersion()`, which hits the `/version` endpoint on the API server. This is the lightest possible authenticated Kubernetes API call. The round-trip latency is measured and included in the response.

---

## Endpoint

| URL | Response | Use case |
|-----|----------|----------|
| `/healthz` | JSON | Scripts, monitoring tools, alerting pipelines |
| `/healthz?format=html` | HTML dashboard | Browser — green/red indicator, auto-refreshes every 10s |

---

## HTTP Status Codes

| Code | Meaning |
|------|---------|
| `200 OK` | API server responded successfully |
| `503 Service Unavailable` | API server unreachable or returned an error |

---

## Running the Tool

```bash
cd ~/tyk-sre-assignment/golang
go run main.go --kubeconfig ~/.kube/config
```

---

## Live Output — JSON

```bash
curl -s http://localhost:8080/healthz | jq .
```

**When reachable (200):**
```json
{
  "status": "ok",
  "apiServer": "reachable",
  "latencyMs": 1,
  "checkedAt": "2026-06-04T18:39:13Z"
}
```

**When unreachable (503):**
```json
{
  "status": "degraded",
  "apiServer": "unreachable",
  "latencyMs": 0,
  "error": "dial tcp: connection refused",
  "checkedAt": "2026-06-04T18:39:13Z"
}
```

The `checkedAt` field is always UTC. The `error` field is omitted from the JSON when the API is reachable.

---

## Live Output — HTML Dashboard

`http://localhost:8080/healthz?format=html`


The dashboard shows:
- A large **green dot** and "API Server Reachable" when the probe succeeds
- A large **red dot** and "API Server Unreachable" when the probe fails
- Probe latency in milliseconds
- UTC timestamp of the last check
- Auto-refreshes every 10 seconds — no manual reload needed
- Footer links to switch between JSON and HTML views

---

## Test Results

```
=== RUN   TestHealthHandler
--- PASS: TestHealthHandler (0.00s)
=== RUN   TestHealthzHandler_APIReachable
--- PASS: TestHealthzHandler_APIReachable (0.00s)
=== RUN   TestHealthzHandler_HTML_200
--- PASS: TestHealthzHandler_HTML_200 (0.01s)
=== RUN   TestHealthzHandler_APIUnreachable
--- PASS: TestHealthzHandler_APIUnreachable (0.00s)
=== RUN   TestHealthzHandler_HTML_503
--- PASS: TestHealthzHandler_HTML_503 (0.00s)
PASS
ok      github.com/TykTechnologies/tyk-sre-assignment   0.089s
```

The unreachable tests use a custom `errorClientset` that wraps the fake clientset and overrides `Discovery().ServerVersion()` to return an error — simulating a cluster that is down without needing a real network failure.

---

## Running the Tests

```bash
cd golang
go test ./... -v
```

No cluster required — all tests use an in-memory fake Kubernetes client.

---

## Implementation Notes

- **The probe call:** `clientset.Discovery().ServerVersion()` calls `GET /version` on the API server. It is the lightest authenticated call available — it returns a small JSON object and requires no list permissions beyond being authenticated.
- **Latency measurement:** `time.Now()` is recorded before the probe and `time.Since(start).Milliseconds()` is calculated after. This gives the true round-trip time including network and API server processing.
- **Handler factory pattern:** `healthHandler` is a factory function — it accepts the `clientset` and returns an `http.HandlerFunc`. This is the same pattern used by `deploymentsHealthHandler`, keeping the codebase consistent.
- **HTML rendering uses `html/template`:** All values inserted into the HTML page are automatically escaped, preventing XSS attacks.
- **Auto-refresh uses no JavaScript:** A `<meta http-equiv="refresh" content="10">` tag handles the reload — no client-side code needed.
