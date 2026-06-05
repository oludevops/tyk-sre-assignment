package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	disco "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/fake"
)

// ---------------------------------------------------------------------------
// getKubernetesVersion
// ---------------------------------------------------------------------------

func TestGetKubernetesVersion(t *testing.T) {
	okClientset := fake.NewSimpleClientset()
	okClientset.Discovery().(*disco.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "1.25.0-fake"}

	okVer, err := getKubernetesVersion(okClientset)
	assert.NoError(t, err)
	assert.Equal(t, "1.25.0-fake", okVer)

	badClientset := fake.NewSimpleClientset()
	badClientset.Discovery().(*disco.FakeDiscovery).FakedServerVersion = &version.Info{}

	badVer, err := getKubernetesVersion(badClientset)
	assert.NoError(t, err)
	assert.Equal(t, "", badVer)
}

// ---------------------------------------------------------------------------
// healthHandler
// ---------------------------------------------------------------------------

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthHandler(fake.NewSimpleClientset())(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Contains(t, res.Header.Get("Content-Type"), "application/json")

	var body HealthzStatus
	assert.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	assert.Equal(t, "ok", body.Status)
	assert.Equal(t, "reachable", body.APIServer)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func int32Ptr(i int32) *int32 { return &i }

func makeDeployment(namespace, name string, desired, ready int32) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(desired)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

// indexByName converts []DeploymentStatus into a map keyed by name,
// avoiding reliance on API ordering in assertions.
func indexByName(statuses []DeploymentStatus) map[string]DeploymentStatus {
	m := make(map[string]DeploymentStatus, len(statuses))
	for _, s := range statuses {
		m[s.Name] = s
	}
	return m
}

// ---------------------------------------------------------------------------
// getDeploymentsHealth
// ---------------------------------------------------------------------------

func TestGetDeploymentsHealth_NoDeployments(t *testing.T) {
	report, err := getDeploymentsHealth(context.Background(), fake.NewSimpleClientset())
	require.NoError(t, err)
	assert.True(t, report.AllHealthy)
	assert.Empty(t, report.Deployments)
}

func TestGetDeploymentsHealth_AllHealthy(t *testing.T) {
	d1 := makeDeployment("default", "api", 3, 3)
	d2 := makeDeployment("monitoring", "prometheus", 1, 1)
	clientset := fake.NewSimpleClientset(&d1, &d2)

	report, err := getDeploymentsHealth(context.Background(), clientset)
	require.NoError(t, err)
	assert.True(t, report.AllHealthy)
	assert.Len(t, report.Deployments, 2)

	for _, ds := range report.Deployments {
		assert.True(t, ds.Healthy)
		assert.Equal(t, ds.DesiredReplicas, ds.ReadyReplicas)
	}
}

func TestGetDeploymentsHealth_PartiallyUnhealthy(t *testing.T) {
	healthy  := makeDeployment("default", "api", 2, 2)
	unhealthy := makeDeployment("default", "worker", 3, 1)
	clientset := fake.NewSimpleClientset(&healthy, &unhealthy)

	report, err := getDeploymentsHealth(context.Background(), clientset)
	require.NoError(t, err)
	assert.False(t, report.AllHealthy)

	byName := indexByName(report.Deployments)
	assert.True(t, byName["api"].Healthy)
	assert.False(t, byName["worker"].Healthy)
	assert.Equal(t, int32(3), byName["worker"].DesiredReplicas)
	assert.Equal(t, int32(1), byName["worker"].ReadyReplicas)
}

// TestGetDeploymentsHealth_ZeroDesiredReplicas verifies that a deployment scaled
// to zero is reported as unhealthy — no pods means no traffic can be served.
func TestGetDeploymentsHealth_ZeroDesiredReplicas(t *testing.T) {
	scaled := makeDeployment("default", "batch-job", 0, 0)
	clientset := fake.NewSimpleClientset(&scaled)

	report, err := getDeploymentsHealth(context.Background(), clientset)
	require.NoError(t, err)
	assert.False(t, report.AllHealthy)
	assert.False(t, indexByName(report.Deployments)["batch-job"].Healthy)
}

func TestGetDeploymentsHealth_MultiNamespace(t *testing.T) {
	d1 := makeDeployment("team-a", "frontend", 2, 2)
	d2 := makeDeployment("team-b", "backend", 2, 0)
	clientset := fake.NewSimpleClientset(&d1, &d2)

	report, err := getDeploymentsHealth(context.Background(), clientset)
	require.NoError(t, err)
	assert.False(t, report.AllHealthy)
	assert.Len(t, report.Deployments, 2)
}

// ---------------------------------------------------------------------------
// deploymentsHealthHandler — JSON
// ---------------------------------------------------------------------------

func TestDeploymentsHealthHandler_AllHealthy(t *testing.T) {
	d := makeDeployment("default", "api", 2, 2)
	clientset := fake.NewSimpleClientset(&d)

	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()
	deploymentsHealthHandler(clientset)(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "application/json", res.Header.Get("Content-Type"))

	var report DeploymentsHealthReport
	require.NoError(t, json.NewDecoder(res.Body).Decode(&report))
	assert.True(t, report.AllHealthy)
	assert.Len(t, report.Deployments, 1)
}

func TestDeploymentsHealthHandler_Unhealthy(t *testing.T) {
	d := makeDeployment("default", "api", 3, 1)
	clientset := fake.NewSimpleClientset(&d)

	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()
	deploymentsHealthHandler(clientset)(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, res.StatusCode)

	var report DeploymentsHealthReport
	require.NoError(t, json.NewDecoder(res.Body).Decode(&report))
	assert.False(t, report.AllHealthy)
}

func TestDeploymentsHealthHandler_NoDeployments(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/deployments/health", nil)
	rec := httptest.NewRecorder()
	deploymentsHealthHandler(fake.NewSimpleClientset())(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)

	var report DeploymentsHealthReport
	require.NoError(t, json.NewDecoder(res.Body).Decode(&report))
	assert.True(t, report.AllHealthy)
	assert.Empty(t, report.Deployments)
}

// ---------------------------------------------------------------------------
// deploymentsHealthHandler — HTML
// ---------------------------------------------------------------------------

func TestDeploymentsHealthHandler_HTML_200(t *testing.T) {
	d := makeDeployment("default", "api", 2, 2)
	clientset := fake.NewSimpleClientset(&d)

	req := httptest.NewRequest(http.MethodGet, "/deployments/health?format=html", nil)
	rec := httptest.NewRecorder()
	deploymentsHealthHandler(clientset)(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Contains(t, res.Header.Get("Content-Type"), "text/html")
}

func TestDeploymentsHealthHandler_HTML_503(t *testing.T) {
	d := makeDeployment("sre-test", "broken-app", 9, 4)
	clientset := fake.NewSimpleClientset(&d)

	req := httptest.NewRequest(http.MethodGet, "/deployments/health?format=html", nil)
	rec := httptest.NewRecorder()
	deploymentsHealthHandler(clientset)(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, res.StatusCode)
	assert.Contains(t, res.Header.Get("Content-Type"), "text/html")
}

// ---------------------------------------------------------------------------
// healthHandler — /healthz
// ---------------------------------------------------------------------------

func TestHealthzHandler_APIReachable(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	healthHandler(fake.NewSimpleClientset())(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Contains(t, res.Header.Get("Content-Type"), "application/json")

	var body HealthzStatus
	assert.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	assert.Equal(t, "ok", body.Status)
	assert.Equal(t, "reachable", body.APIServer)
	assert.Empty(t, body.Error)
}

func TestHealthzHandler_HTML_200(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz?format=html", nil)
	rec := httptest.NewRecorder()
	healthHandler(fake.NewSimpleClientset())(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Contains(t, res.Header.Get("Content-Type"), "text/html")
}

// errorDiscovery always returns an error from ServerVersion(),
// simulating an unreachable Kubernetes API server.
type errorDiscovery struct {
	*disco.FakeDiscovery
}

func (e *errorDiscovery) ServerVersion() (*version.Info, error) {
	return nil, fmt.Errorf("dial tcp: connection refused")
}

// errorClientset wraps fake.Clientset and replaces Discovery() with errorDiscovery.
type errorClientset struct {
	*fake.Clientset
}

func (e *errorClientset) Discovery() discovery.DiscoveryInterface {
	return &errorDiscovery{FakeDiscovery: e.Clientset.Discovery().(*disco.FakeDiscovery)}
}

func TestHealthzHandler_APIUnreachable(t *testing.T) {
	client := &errorClientset{Clientset: fake.NewSimpleClientset()}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	healthHandler(client)(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, res.StatusCode)
	assert.Contains(t, res.Header.Get("Content-Type"), "application/json")

	var body HealthzStatus
	assert.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	assert.Equal(t, "degraded", body.Status)
	assert.Equal(t, "unreachable", body.APIServer)
	assert.NotEmpty(t, body.Error)
}

func TestHealthzHandler_HTML_503(t *testing.T) {
	client := &errorClientset{Clientset: fake.NewSimpleClientset()}

	req := httptest.NewRequest(http.MethodGet, "/healthz?format=html", nil)
	rec := httptest.NewRecorder()
	healthHandler(client)(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, res.StatusCode)
	assert.Contains(t, res.Header.Get("Content-Type"), "text/html")
}
