// This file contains all the automated tests for the SRE tool.
//
// In Go, test files end with _test.go. The Go toolchain compiles and runs them
// with "go test ./...". Test functions must be named Test<Something> and accept
// a *testing.T argument — that's what gives you t.Error(), t.Fatal(), etc.
//
// We use two external test helper libraries:
//   - github.com/stretchr/testify/assert  – soft assertions (test continues on failure)
//   - github.com/stretchr/testify/require – hard assertions (test stops on failure)
//
// For the Kubernetes client we use a *fake* clientset. Instead of calling a real
// cluster, the fake clientset stores objects in memory and returns them when queried.
// This makes our tests fast, isolated, and runnable without a cluster.
package main

import (
	// "context" provides context.Background(), a no-op context used when we have
	// no deadline or cancellation to propagate.
	"context"

	// "encoding/json" decodes the JSON response body in the handler tests.
	"encoding/json"

	// "io" provides io.ReadCloser, used when closing the HTTP response body.
	"io"

	// Standard HTTP types and the httptest package for creating in-memory HTTP
	// requests and response recorders — no real network needed.
	"net/http"
	"net/http/httptest"
	"testing"

	// Testify assertion helpers.
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// Kubernetes API types for the apps/v1 group, which contains Deployment.
	appsv1 "k8s.io/api/apps/v1"

	// metav1 contains ObjectMeta (name, namespace, labels, etc.) and ListOptions.
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	// version.Info is the struct returned by the discovery API — used to fake the server version.
	"k8s.io/apimachinery/pkg/version"

	// disco is the fake implementation of the Kubernetes Discovery interface.
	// It lets us set a pretend server version without a real cluster.
	disco "k8s.io/client-go/discovery/fake"

	// fake provides fake.NewSimpleClientset(), which returns an in-memory
	// Kubernetes client pre-loaded with whatever objects we pass to it.
	"k8s.io/client-go/kubernetes/fake"
)

// ---------------------------------------------------------------------------
// Tests for getKubernetesVersion
// ---------------------------------------------------------------------------

// TestGetKubernetesVersion checks that getKubernetesVersion correctly reads
// the server version from the Kubernetes discovery API.
func TestGetKubernetesVersion(t *testing.T) {
	// Create a fake clientset — an in-memory Kubernetes client with no real objects yet.
	okClientset := fake.NewSimpleClientset()

	// Override the fake discovery client's version so it reports "1.25.0-fake".
	// The type assertion (*disco.FakeDiscovery) is needed because the interface
	// doesn't expose FakedServerVersion; we have to cast to the concrete type first.
	okClientset.Discovery().(*disco.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "1.25.0-fake"}

	// Call the real function we want to test.
	okVer, err := getKubernetesVersion(okClientset)

	// assert.NoError checks that err is nil. If it isn't, the test fails but continues.
	assert.NoError(t, err)
	// assert.Equal checks that the two values are identical.
	assert.Equal(t, "1.25.0-fake", okVer)

	// Now test the edge case where the version info struct is empty.
	// version.Info{} has all zero values, so GitVersion is "".
	badClientset := fake.NewSimpleClientset()
	badClientset.Discovery().(*disco.FakeDiscovery).FakedServerVersion = &version.Info{}

	badVer, err := getKubernetesVersion(badClientset)
	assert.NoError(t, err)
	assert.Equal(t, "", badVer)
}

// ---------------------------------------------------------------------------
// Tests for healthHandler
// ---------------------------------------------------------------------------

// TestHealthHandler verifies that GET /healthz returns HTTP 200 with body "ok".
func TestHealthHandler(t *testing.T) {
	// httptest.NewRequest builds an *http.Request without opening a real network socket.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	// httptest.NewRecorder is a fake http.ResponseWriter that captures everything
	// written to it so we can inspect the status code and body in assertions.
	rec := httptest.NewRecorder()

	// Call the handler directly — no HTTP server needed.
	healthHandler(rec, req)

	// Result() converts the recorder into a standard *http.Response.
	res := rec.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)

	// Always close the response body when you're done — it's a good habit even
	// with in-memory responses, and required by the http.Response contract.
	defer func(Body io.ReadCloser) {
		assert.NoError(t, Body.Close())
	}(res.Body)

	// Read the entire body as a byte slice, then compare as a string.
	resp, err := io.ReadAll(res.Body)
	assert.NoError(t, err)
	assert.Equal(t, "ok", string(resp))
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// int32Ptr returns a pointer to the given int32 value.
//
// Kubernetes uses *int32 (a pointer) for Spec.Replicas so it can distinguish
// between "explicitly set to 0" and "not set at all" (nil). Since Go doesn't
// let you take the address of a literal directly (e.g. &int32(3) is invalid),
// we need this small helper function.
func int32Ptr(i int32) *int32 { return &i }

// makeDeployment constructs a minimal appsv1.Deployment for use in tests.
//
// We only populate the fields our code actually reads:
//   - ObjectMeta.Name and .Namespace — to identify the deployment
//   - Spec.Replicas                  — the desired pod count
//   - Status.ReadyReplicas           — the current healthy pod count
//
// A real Deployment has dozens more fields, but we don't need them for these tests.
func makeDeployment(namespace, name string, desired int32, ready int32) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(desired),
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: ready,
		},
	}
}

// ---------------------------------------------------------------------------
// Tests for getDeploymentsHealth
// ---------------------------------------------------------------------------

// TestGetDeploymentsHealth_NoDeployments checks that an empty cluster is reported as healthy.
// "No news is good news" — if there are no deployments, nothing is broken.
func TestGetDeploymentsHealth_NoDeployments(t *testing.T) {
	// An empty fake clientset — no Kubernetes objects at all.
	clientset := fake.NewSimpleClientset()

	report, err := getDeploymentsHealth(context.Background(), clientset)

	// require.NoError is a *hard* assertion — if err != nil the test stops here.
	// We use require when subsequent assertions would panic on a nil report.
	require.NoError(t, err)
	assert.True(t, report.AllHealthy, "cluster with no deployments should be considered healthy")
	assert.Empty(t, report.Deployments) // the list should be empty, not nil
}

// TestGetDeploymentsHealth_AllHealthy checks that the report is healthy
// when every deployment has all its requested replicas ready.
func TestGetDeploymentsHealth_AllHealthy(t *testing.T) {
	// Two deployments in different namespaces, both fully ready.
	d1 := makeDeployment("default", "api", 3, 3)         // 3 desired, 3 ready
	d2 := makeDeployment("monitoring", "prometheus", 1, 1) // 1 desired, 1 ready

	// Pass both deployments to the fake clientset so they are returned when listed.
	clientset := fake.NewSimpleClientset(&d1, &d2)

	report, err := getDeploymentsHealth(context.Background(), clientset)

	require.NoError(t, err)
	assert.True(t, report.AllHealthy)
	assert.Len(t, report.Deployments, 2) // both deployments should appear in the report

	// Check every entry individually — they should all be healthy.
	for _, ds := range report.Deployments {
		assert.True(t, ds.Healthy, "deployment %s/%s should be healthy", ds.Namespace, ds.Name)
		assert.Equal(t, ds.DesiredReplicas, ds.ReadyReplicas)
	}
}

// TestGetDeploymentsHealth_PartiallyUnhealthy checks that a single degraded deployment
// flips the top-level AllHealthy flag to false, even when other deployments are fine.
func TestGetDeploymentsHealth_PartiallyUnhealthy(t *testing.T) {
	healthy  := makeDeployment("default", "api",    2, 2) // all good
	unhealthy := makeDeployment("default", "worker", 3, 1) // only 1 of 3 ready — degraded

	clientset := fake.NewSimpleClientset(&healthy, &unhealthy)

	report, err := getDeploymentsHealth(context.Background(), clientset)

	require.NoError(t, err)
	assert.False(t, report.AllHealthy, "report should be unhealthy when any deployment is degraded")

	// indexByName converts the slice into a map so we can look up by name
	// instead of relying on ordering (the API doesn't guarantee order).
	byName := indexByName(report.Deployments)

	// The "api" deployment should be healthy.
	assert.True(t, byName["api"].Healthy)
	assert.Equal(t, int32(2), byName["api"].DesiredReplicas)
	assert.Equal(t, int32(2), byName["api"].ReadyReplicas)

	// The "worker" deployment should be unhealthy — 3 desired but only 1 ready.
	assert.False(t, byName["worker"].Healthy)
	assert.Equal(t, int32(3), byName["worker"].DesiredReplicas)
	assert.Equal(t, int32(1), byName["worker"].ReadyReplicas)
}

// TestGetDeploymentsHealth_ZeroDesiredReplicas checks that a deployment explicitly
// scaled to zero replicas is reported as *unhealthy*.
//
// Rationale: if desired == 0, no pods are running, so the workload cannot serve traffic.
// An operator may have intentionally scaled it down, but from a reliability perspective
// it is not healthy. The SRE needs to know about it.
func TestGetDeploymentsHealth_ZeroDesiredReplicas(t *testing.T) {
	scaled := makeDeployment("default", "batch-job", 0, 0) // scaled to zero

	clientset := fake.NewSimpleClientset(&scaled)

	report, err := getDeploymentsHealth(context.Background(), clientset)

	require.NoError(t, err)
	assert.False(t, report.AllHealthy)

	byName := indexByName(report.Deployments)
	assert.False(t, byName["batch-job"].Healthy)
}

// TestGetDeploymentsHealth_MultiNamespace verifies that deployments from different
// namespaces are all included in the report and evaluated correctly.
func TestGetDeploymentsHealth_MultiNamespace(t *testing.T) {
	d1 := makeDeployment("team-a", "frontend", 2, 2) // healthy
	d2 := makeDeployment("team-b", "backend",  2, 0) // completely down — 0 of 2 ready

	clientset := fake.NewSimpleClientset(&d1, &d2)

	report, err := getDeploymentsHealth(context.Background(), clientset)

	require.NoError(t, err)
	assert.False(t, report.AllHealthy)          // backend is down
	assert.Len(t, report.Deployments, 2)         // both namespaces present
}

// ---------------------------------------------------------------------------
// Tests for deploymentsHealthHandler (the HTTP layer)
// ---------------------------------------------------------------------------

// TestDeploymentsHealthHandler_AllHealthy verifies that the HTTP handler returns
// 200 OK with a valid JSON body when all deployments are healthy.
func TestDeploymentsHealthHandler_AllHealthy(t *testing.T) {
	d := makeDeployment("default", "api", 2, 2) // fully ready
	clientset := fake.NewSimpleClientset(&d)

	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()

	// deploymentsHealthHandler(clientset) returns the handler function,
	// which we then call immediately with (rec, req).
	deploymentsHealthHandler(clientset)(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "application/json", res.Header.Get("Content-Type"))

	// Decode the JSON response body into our report struct and inspect it.
	var report DeploymentsHealthReport
	require.NoError(t, json.NewDecoder(res.Body).Decode(&report))
	assert.True(t, report.AllHealthy)
	assert.Len(t, report.Deployments, 1)
}

// TestDeploymentsHealthHandler_Unhealthy verifies that the HTTP handler returns
// 503 Service Unavailable when at least one deployment is degraded.
func TestDeploymentsHealthHandler_Unhealthy(t *testing.T) {
	d := makeDeployment("default", "api", 3, 1) // 3 desired, only 1 ready
	clientset := fake.NewSimpleClientset(&d)

	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()

	deploymentsHealthHandler(clientset)(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	// The status code must be 503, not 200.
	assert.Equal(t, http.StatusServiceUnavailable, res.StatusCode)

	var report DeploymentsHealthReport
	require.NoError(t, json.NewDecoder(res.Body).Decode(&report))
	assert.False(t, report.AllHealthy)
}

// TestDeploymentsHealthHandler_NoDeployments verifies that an empty cluster
// returns 200 OK (not an error) with an empty deployments list.
func TestDeploymentsHealthHandler_NoDeployments(t *testing.T) {
	clientset := fake.NewSimpleClientset() // empty cluster

	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()

	deploymentsHealthHandler(clientset)(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	// No deployments means nothing is broken — expect 200, not 503.
	assert.Equal(t, http.StatusOK, res.StatusCode)

	var report DeploymentsHealthReport
	require.NoError(t, json.NewDecoder(res.Body).Decode(&report))
	assert.True(t, report.AllHealthy)
	assert.Empty(t, report.Deployments)
}

// ---------------------------------------------------------------------------
// Shared test utility
// ---------------------------------------------------------------------------

// indexByName converts a []DeploymentStatus slice into a map[name]DeploymentStatus.
// This makes it easy to look up a specific deployment by name in assertions
// without depending on the order the API returns them.
func indexByName(statuses []DeploymentStatus) map[string]DeploymentStatus {
	m := make(map[string]DeploymentStatus, len(statuses))
	for _, s := range statuses {
		m[s.Name] = s
	}
	return m
}
