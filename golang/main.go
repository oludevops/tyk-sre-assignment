// Package main is the entry point for the SRE tool.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig, leave empty for in-cluster")
	flag.Parse()

	// If no kubeconfig is provided, the tool is running inside the cluster.
	// Use the in-cluster service account token and skip the interactive prompt.
	if *kubeconfig == "" {
		fmt.Println("Running inside cluster — using in-cluster service account token.")
		config, err := rest.InClusterConfig()
		if err != nil {
			panic(fmt.Errorf("in-cluster config failed: %w", err))
		}
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			panic(err)
		}
		version, err := getKubernetesVersion(clientset)
		if err != nil {
			panic(err)
		}
		fmt.Printf("Connected to Kubernetes %s\n", version)
		if err := startServer(":8080", clientset, "in-cluster"); err != nil {
			panic(err)
		}
		return
	}

	// Print immediately so the operator knows the tool has started.
	fmt.Println("SRE Tool starting — reading cluster configuration...")

	// Run dots in the background while listContexts loads the kubeconfig.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				fmt.Print(".")
				time.Sleep(300 * time.Millisecond)
			}
		}
	}()

	// List all contexts available in the kubeconfig.
	contexts, err := listContexts(*kubeconfig)

	// Stop the dots, clear the dot line, move to a new line.
	close(done)
	time.Sleep(350 * time.Millisecond) // give the goroutine time to stop
	fmt.Print("\r                    \r") // overwrite any printed dots with spaces

	if err != nil {
		panic(err)
	}

	// Ask the operator which cluster(s) to monitor.
	selected := promptClusterSelection(contexts)

	// Start one server per selected cluster, each on its own port.
	var wg sync.WaitGroup
	startPort := 8080

	for _, ctx := range selected {
		// Build a clientset for this context.
		clientset, err := buildClientsetForContext(*kubeconfig, ctx)
		if err != nil {
			fmt.Printf("  error connecting to %s: %v\n", ctx, err)
			continue
		}

		// Verify connectivity.
		version, err := getKubernetesVersion(clientset)
		if err != nil {
			fmt.Printf("  error reaching API server for %s: %v\n", ctx, err)
			continue
		}

		// Find the next available port.
		port, err := findAvailablePort(startPort)
		if err != nil {
			fmt.Printf("  no available port for %s: %v\n", ctx, err)
			continue
		}
		startPort = port + 1 // next cluster starts searching from the port after this one

		addr := fmt.Sprintf(":%d", port)
		fmt.Println("=================================================================")
		fmt.Printf("  Cluster: %s\n", ctx)
		fmt.Printf("  URL:     http://localhost%s  (Kubernetes %s)\n", addr, version)
		fmt.Printf("\n  From a second terminal run the following commands:\n")
		fmt.Printf("    curl -s http://localhost%s/deployments/health | jq .\n", addr)
		fmt.Printf("    curl -s http://localhost%s/healthz | jq .\n", addr)
		fmt.Printf("    Browser: http://localhost%s/deployments/health?format=html\n", addr)
		fmt.Printf("    Browser: http://localhost%s/healthz?format=html\n", addr)

		wg.Add(1)
		go func(address string, cs kubernetes.Interface, name string) {
			defer wg.Done()
			if err := startServer(address, cs, name); err != nil {
				fmt.Printf("server error on %s: %v\n", address, err)
			}
		}(addr, clientset, ctx)
	}

	wg.Wait()
}

// listContexts reads all context names from the kubeconfig file.
// If no kubeconfig path is given it uses the default discovery rules
// (KUBECONFIG env var, then ~/.kube/config).
func listContexts(kubeconfigPath string) ([]string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}

	config, err := loadingRules.Load()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	contexts := make([]string, 0, len(config.Contexts))
	for name := range config.Contexts {
		contexts = append(contexts, name)
	}
	sort.Strings(contexts)
	return contexts, nil
}

// promptClusterSelection prints the list of available contexts and asks the
// operator to select one or all. Returns the slice of selected context names.
func promptClusterSelection(contexts []string) []string {
	fmt.Println("\nAvailable clusters:")
	for i, ctx := range contexts {
		fmt.Printf("  [%d] %s\n", i+1, ctx)
	}
	fmt.Printf("  [a] All clusters\n")
	fmt.Printf("\nSelect a cluster (1-%d) or 'a' for all: ", len(contexts))

	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	if input == "a" || input == "all" {
		fmt.Println("\nStarting server for all clusters...")
		return contexts
	}

	for {
		n, err := strconv.Atoi(input)
		if err == nil && n >= 1 && n <= len(contexts) {
			return []string{contexts[n-1]}
		}
		fmt.Printf("Invalid selection %q — please enter a number between 1 and %d or 'a' for all: ", input, len(contexts))
		input, _ = reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input == "a" || input == "all" {
			fmt.Println("\nStarting server for all clusters...")
			return contexts
		}
	}
}

// buildClientsetForContext builds a Kubernetes clientset for a specific
// named context in the kubeconfig.
func buildClientsetForContext(kubeconfigPath, contextName string) (kubernetes.Interface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}

	kConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building config for context %s: %w", contextName, err)
	}

	return kubernetes.NewForConfig(kConfig)
}

// findAvailablePort tries ports starting from start until it finds one that
// is not already in use. It tries up to 100 ports before giving up.
func findAvailablePort(start int) (int, error) {
	for port := start; port < start+100; port++ {
		addr := fmt.Sprintf(":%d", port)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return port, nil
		}
		fmt.Printf("  :%d in use — skipping\n", port)
	}
	return 0, fmt.Errorf("no available port found in range %d-%d", start, start+100)
}

// getKubernetesVersion returns the GitVersion of the Kubernetes API server.
// It also serves as a connectivity check on startup.
func getKubernetesVersion(clientset kubernetes.Interface) (string, error) {
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return version.String(), nil
}

// startServer registers HTTP routes and starts listening. It blocks until the server stops.
// Each call creates its own ServeMux so multiple servers can run concurrently
// without conflicting over the global default mux.
func startServer(listenAddr string, clientset kubernetes.Interface, clusterName string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler(clientset, clusterName))
	mux.HandleFunc("/deployments/health", deploymentsHealthHandler(clientset, clusterName))
	fmt.Printf("Server listening on %s\n", listenAddr)
	fmt.Println("=================================================================")
	return http.ListenAndServe(listenAddr, mux)
}

// HealthzStatus is the JSON response body for GET /healthz.
type HealthzStatus struct {
	Cluster   string `json:"cluster"`
	Status    string `json:"status"`
	APIServer string `json:"apiServer"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
	CheckedAt string `json:"checkedAt"`
}

// healthzHTMLTemplate is the HTML dashboard for GET /healthz?format=html.
var healthzHTMLTemplate = template.Must(template.New("healthz").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>API Server Health</title>
	<meta http-equiv="refresh" content="10">
	<style>
		*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
		body {
			font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
			font-size: 16px;
			color: #000;
			background: #f5f5f5;
			padding: 32px;
		}
		h1 { font-size: 26px; font-weight: 600; margin-bottom: 24px; color: #000; }
		.status-card {
			background: #fff;
			border: 1px solid #e0e0e0;
			border-radius: 6px;
			padding: 32px 40px;
			display: inline-flex;
			flex-direction: column;
			gap: 20px;
			margin-bottom: 28px;
			min-width: 380px;
		}
		.status-indicator { display: flex; align-items: center; gap: 14px; font-size: 24px; font-weight: 700; }
		.dot { width: 20px; height: 20px; border-radius: 50%; flex-shrink: 0; }
		.dot.ok       { background: #16a34a; }
		.dot.degraded { background: #dc2626; }
		.status-text.ok       { color: #16a34a; }
		.status-text.degraded { color: #dc2626; }
		.detail-row { display: flex; gap: 12px; font-size: 16px; color: #000; }
		.detail-label { font-weight: 600; min-width: 100px; color: #000; }
		.detail-value { color: #000; }
		.error-box {
			background: #fef2f2;
			border: 1px solid #fecaca;
			border-radius: 4px;
			padding: 12px 16px;
			font-size: 15px;
			color: #dc2626;
			font-family: "Courier New", Consolas, monospace;
			word-break: break-all;
		}
		.footer { margin-top: 16px; font-size: 16px; font-weight: 500; color: #000; }
		.footer a       { color: #000; font-weight: 600; text-decoration: none; }
		.footer a:hover { text-decoration: underline; }
	</style>
</head>
<body>
<h1>API Server Health — {{ .Cluster }}</h1>
<div class="status-card">
	{{ if eq .Status "ok" }}
	<div class="status-indicator">
		<div class="dot ok"></div>
		<span class="status-text ok">API Server Reachable</span>
	</div>
	{{ else }}
	<div class="status-indicator">
		<div class="dot degraded"></div>
		<span class="status-text degraded">API Server Unreachable</span>
	</div>
	{{ end }}
	<div class="detail-row">
		<span class="detail-label">Latency</span>
		<span class="detail-value">{{ .LatencyMs }} ms</span>
	</div>
	<div class="detail-row">
		<span class="detail-label">Checked at</span>
		<span class="detail-value">{{ .CheckedAt }}</span>
	</div>
	{{ if .Error }}
	<div class="error-box">{{ .Error }}</div>
	{{ end }}
</div>
<p class="footer">
	Auto-refreshes every 10s &mdash;
	<a href="/healthz">JSON</a> &middot;
	HTML
</p>
</body>
</html>
`))

// healthHandler probes the Kubernetes API server on every request and returns
// 200 when reachable, 503 when not. Supports ?format=html for a visual dashboard.
func healthHandler(clientset kubernetes.Interface, clusterName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		_, err := clientset.Discovery().ServerVersion()
		latencyMs := time.Since(start).Milliseconds()

		var status HealthzStatus
		var statusCode int

		if err == nil {
			status = HealthzStatus{
				Cluster:   clusterName,
				Status:    "ok",
				APIServer: "reachable",
				LatencyMs: latencyMs,
				CheckedAt: time.Now().UTC().Format(time.RFC3339),
			}
			statusCode = http.StatusOK
		} else {
			status = HealthzStatus{
				Cluster:   clusterName,
				Status:    "degraded",
				APIServer: "unreachable",
				LatencyMs: 0,
				Error:     err.Error(),
				CheckedAt: time.Now().UTC().Format(time.RFC3339),
			}
			statusCode = http.StatusServiceUnavailable
		}

		if r.URL.Query().Get("format") == "html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(statusCode)
			if tmplErr := healthzHTMLTemplate.Execute(w, status); tmplErr != nil {
				fmt.Printf("failed rendering healthz template: %v\n", tmplErr)
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if jsonErr := json.NewEncoder(w).Encode(status); jsonErr != nil {
			fmt.Printf("failed encoding healthz JSON: %v\n", jsonErr)
		}
	}
}

// DeploymentStatus holds the health information for a single Deployment.
type DeploymentStatus struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	DesiredReplicas int32  `json:"desiredReplicas"`
	ReadyReplicas   int32  `json:"readyReplicas"`
	Healthy         bool   `json:"healthy"`
}

// DeploymentsHealthReport is the top-level response for GET /deployments/health.
type DeploymentsHealthReport struct {
	Cluster     string             `json:"cluster"`
	Deployments []DeploymentStatus `json:"deployments"`
	AllHealthy  bool               `json:"allHealthy"`
}

// getDeploymentsHealth lists all Deployments across every namespace and evaluates
// whether each one has the expected number of ready pods.
func getDeploymentsHealth(ctx context.Context, clientset kubernetes.Interface, clusterName string) (*DeploymentsHealthReport, error) {
	deploymentList, err := clientset.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}

	report := &DeploymentsHealthReport{
		Cluster:     clusterName,
		Deployments: make([]DeploymentStatus, 0, len(deploymentList.Items)),
		AllHealthy:  true,
	}

	for _, d := range deploymentList.Items {
		// Spec.Replicas is a pointer — nil means the Kubernetes default of 1.
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}

		ready := d.Status.ReadyReplicas
		healthy := desired > 0 && ready == desired

		if !healthy {
			report.AllHealthy = false
		}

		report.Deployments = append(report.Deployments, DeploymentStatus{
			Name:            d.Name,
			Namespace:       d.Namespace,
			DesiredReplicas: desired,
			ReadyReplicas:   ready,
			Healthy:         healthy,
		})
	}

	return report, nil
}

// deploymentsHealthHandler returns deployment health across all namespaces.
// Supports ?format=html for a dashboard view and ?format=table for a spreadsheet view.
// Returns 200 when all deployments are healthy, 503 when any are degraded.
func deploymentsHealthHandler(clientset kubernetes.Interface, clusterName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report, err := getDeploymentsHealth(r.Context(), clientset, clusterName)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to retrieve deployments: %v", err), http.StatusInternalServerError)
			return
		}

		statusCode := http.StatusOK
		if !report.AllHealthy {
			statusCode = http.StatusServiceUnavailable
		}

		switch r.URL.Query().Get("format") {
		case "html":
			renderHTML(w, report, statusCode)
		case "table":
			renderTable(w, report, statusCode)
		default:
			renderJSON(w, report, statusCode)
		}
	}
}

// renderJSON writes the deployment health report as JSON.
func renderJSON(w http.ResponseWriter, report *DeploymentsHealthReport, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(report); err != nil {
		fmt.Println("failed writing JSON response")
	}
}

// htmlTemplateData is passed into the HTML and table templates.
type htmlTemplateData struct {
	Cluster       string
	Deployments   []DeploymentStatus
	Total         int
	HealthyCount  int
	DegradedCount int
}

// htmlTemplate renders the card + badge dashboard for ?format=html.
var htmlTemplate = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>Deployment Health</title>
	<meta http-equiv="refresh" content="10">
	<style>
		*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
		body {
			font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
			font-size: 16px;
			color: #000;
			background: #f5f5f5;
			padding: 32px;
		}
		h1 { font-size: 26px; font-weight: 600; margin-bottom: 24px; color: #000; }
		.cards { display: flex; gap: 16px; margin-bottom: 28px; }
		.card {
			background: #fff;
			border: 1px solid #e0e0e0;
			border-radius: 6px;
			padding: 20px 28px;
			min-width: 120px;
		}
		.card-label {
			font-size: 13px;
			font-weight: 600;
			letter-spacing: 0.08em;
			text-transform: uppercase;
			color: #000;
			margin-bottom: 8px;
		}
		.card-value { font-size: 36px; font-weight: 700; line-height: 1; color: #000; }
		.card-value.healthy  { color: #2d9e5f; }
		.card-value.degraded { color: #d97706; }
		.table-wrapper {
			background: #fff;
			border: 1px solid #e0e0e0;
			border-radius: 6px;
			overflow: hidden;
		}
		table { width: 100%; border-collapse: collapse; }
		thead th {
			padding: 14px 20px;
			text-align: left;
			font-size: 13px;
			font-weight: 700;
			letter-spacing: 0.07em;
			text-transform: uppercase;
			color: #000;
			border-bottom: 1px solid #e0e0e0;
		}
		thead th.num, tbody td.num { text-align: right; }
		tbody td {
			padding: 16px 20px;
			font-size: 16px;
			border-bottom: 1px solid #f0f0f0;
			color: #000;
		}
		tbody tr:last-child td { border-bottom: none; }
		tbody tr:hover { background: #fafafa; }
		.ready-degraded { color: #d97706; font-weight: 600; }
		.badge {
			display: inline-flex;
			align-items: center;
			gap: 6px;
			padding: 4px 12px;
			border-radius: 999px;
			font-size: 13px;
			font-weight: 500;
		}
		.badge::before { content: "●"; font-size: 8px; }
		.badge.healthy  { background: #dcfce7; color: #16a34a; }
		.badge.degraded { background: #fef3c7; color: #d97706; }
		.footer { margin-top: 16px; font-size: 16px; font-weight: 500; color: #000; }
		.footer a       { color: #000; font-weight: 600; text-decoration: none; }
		.footer a:hover { text-decoration: underline; }
	</style>
</head>
<body>
<h1>Deployment Health — {{ .Cluster }}</h1>
<div class="cards">
	<div class="card">
		<div class="card-label">Total</div>
		<div class="card-value">{{ .Total }}</div>
	</div>
	<div class="card">
		<div class="card-label">Healthy</div>
		<div class="card-value healthy">{{ .HealthyCount }}</div>
	</div>
	<div class="card">
		<div class="card-label">Degraded</div>
		<div class="card-value degraded">{{ .DegradedCount }}</div>
	</div>
</div>
<div class="table-wrapper">
	<table>
		<thead>
			<tr>
				<th>Namespace</th>
				<th>Name</th>
				<th class="num">Desired</th>
				<th class="num">Ready</th>
				<th>Status</th>
			</tr>
		</thead>
		<tbody>
			{{ range .Deployments }}
			<tr>
				<td>{{ .Namespace }}</td>
				<td>{{ .Name }}</td>
				<td class="num">{{ .DesiredReplicas }}</td>
				{{ if .Healthy }}
				<td class="num">{{ .ReadyReplicas }}</td>
				<td><span class="badge healthy">Healthy</span></td>
				{{ else }}
				<td class="num ready-degraded">{{ .ReadyReplicas }}</td>
				<td><span class="badge degraded">Degraded</span></td>
				{{ end }}
			</tr>
			{{ end }}
		</tbody>
	</table>
</div>
<p class="footer">
	Auto-refreshes every 10s &mdash;
	<a href="/deployments/health">JSON</a> &middot;
	HTML &middot;
	<a href="/deployments/health?format=table">Table</a>
</p>
</body>
</html>
`))

// renderHTML writes the deployment health report as a card + badge HTML dashboard.
func renderHTML(w http.ResponseWriter, report *DeploymentsHealthReport, statusCode int) {
	healthyCount := 0
	for _, d := range report.Deployments {
		if d.Healthy {
			healthyCount++
		}
	}

	data := htmlTemplateData{
		Cluster:       report.Cluster,
		Deployments:   report.Deployments,
		Total:         len(report.Deployments),
		HealthyCount:  healthyCount,
		DegradedCount: len(report.Deployments) - healthyCount,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusCode)
	if err := htmlTemplate.Execute(w, data); err != nil {
		fmt.Printf("failed rendering HTML template: %v\n", err)
	}
}

// tableTemplate renders the Excel-style spreadsheet view for ?format=table.
var tableTemplate = template.Must(template.New("spreadsheet").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>Deployment Health</title>
	<meta http-equiv="refresh" content="10">
	<style>
		*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
		body {
			font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
			font-size: 16px;
			color: #000;
			background: #f5f5f5;
			padding: 32px;
		}
		h1 { font-size: 26px; font-weight: 600; margin-bottom: 24px; color: #000; }
		.cards { display: flex; gap: 16px; margin-bottom: 28px; }
		.card {
			background: #fff;
			border: 1px solid #e0e0e0;
			border-radius: 6px;
			padding: 20px 28px;
			min-width: 120px;
		}
		.card-label {
			font-size: 13px;
			font-weight: 600;
			letter-spacing: 0.08em;
			text-transform: uppercase;
			color: #000;
			margin-bottom: 8px;
		}
		.card-value        { font-size: 36px; font-weight: 700; line-height: 1; color: #000; }
		.card-value.healthy  { color: #2d9e5f; }
		.card-value.degraded { color: #d97706; }
		.table-wrapper { background: #fff; border: 2px solid #bbb; }
		table { width: 100%; border-collapse: collapse; }
		thead th {
			padding: 12px 16px;
			text-align: left;
			font-size: 13px;
			font-weight: 700;
			letter-spacing: 0.07em;
			text-transform: uppercase;
			color: #000;
			background: #e8e8e8;
			border: 1px solid #bbb;
		}
		thead th.num, tbody td.num { text-align: right; }
		tbody td { padding: 12px 16px; font-size: 16px; color: #000; border: 1px solid #d0d0d0; }
		tbody td.num { font-family: "Courier New", Consolas, monospace; font-size: 15px; }
		tbody tr:nth-child(even) { background: #f7f7f7; }
		tbody tr:nth-child(odd)  { background: #ffffff; }
		tbody tr:hover { background: #eef4ff; }
		.ready-degraded { color: #d97706; font-weight: 700; }
		.status-healthy  { color: #16a34a; font-weight: 600; }
		.status-degraded { color: #d97706; font-weight: 700; }
		.footer { margin-top: 16px; font-size: 16px; font-weight: 500; color: #000; }
		.footer a          { color: #000; font-weight: 600; text-decoration: none; }
		.footer a:hover    { text-decoration: underline; }
		.footer .active    { font-weight: 700; }
	</style>
</head>
<body>
<h1>Deployment Health — {{ .Cluster }}</h1>
<div class="cards">
	<div class="card">
		<div class="card-label">Total</div>
		<div class="card-value">{{ .Total }}</div>
	</div>
	<div class="card">
		<div class="card-label">Healthy</div>
		<div class="card-value healthy">{{ .HealthyCount }}</div>
	</div>
	<div class="card">
		<div class="card-label">Degraded</div>
		<div class="card-value degraded">{{ .DegradedCount }}</div>
	</div>
</div>
<div class="table-wrapper">
	<table>
		<thead>
			<tr>
				<th>Namespace</th>
				<th>Name</th>
				<th class="num">Desired</th>
				<th class="num">Ready</th>
				<th>Status</th>
			</tr>
		</thead>
		<tbody>
			{{ range .Deployments }}
			<tr>
				<td>{{ .Namespace }}</td>
				<td>{{ .Name }}</td>
				<td class="num">{{ .DesiredReplicas }}</td>
				{{ if .Healthy }}
				<td class="num">{{ .ReadyReplicas }}</td>
				<td class="status-healthy">Healthy</td>
				{{ else }}
				<td class="num ready-degraded">{{ .ReadyReplicas }}</td>
				<td class="status-degraded">Degraded</td>
				{{ end }}
			</tr>
			{{ end }}
		</tbody>
	</table>
</div>
<p class="footer">
	Auto-refreshes every 10s &mdash;
	<a href="/deployments/health">JSON</a> &middot;
	<a href="/deployments/health?format=html">HTML</a> &middot;
	<span class="active">Table</span>
</p>
</body>
</html>
`))

// renderTable writes the deployment health report as an Excel-style HTML table.
func renderTable(w http.ResponseWriter, report *DeploymentsHealthReport, statusCode int) {
	healthyCount := 0
	for _, d := range report.Deployments {
		if d.Healthy {
			healthyCount++
		}
	}

	data := htmlTemplateData{
		Cluster:       report.Cluster,
		Deployments:   report.Deployments,
		Total:         len(report.Deployments),
		HealthyCount:  healthyCount,
		DegradedCount: len(report.Deployments) - healthyCount,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusCode)
	if err := tableTemplate.Execute(w, data); err != nil {
		fmt.Printf("failed rendering table template: %v\n", err)
	}
}
