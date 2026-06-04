// Package main is the entry point for the SRE tool.
// This tool connects to a Kubernetes cluster and exposes an HTTP server
// with endpoints that help the SRE team monitor cluster health.
package main

import (
	// "context" lets us pass cancellation signals and deadlines to API calls.
	// Think of it as a way to say "if this takes too long, give up".
	"context"

	// "encoding/json" converts Go structs into JSON text (and back).
	// We use it to write JSON responses to the HTTP client.
	"encoding/json"

	// "flag" parses command-line arguments like --kubeconfig or --address.
	"flag"

	// "fmt" is the standard formatting package — used for Printf, Sprintf, Errorf.
	"fmt"

	// "net/http" is Go's built-in HTTP server library.
	"net/http"

	// metav1 contains common Kubernetes API types, like ListOptions.
	// The alias "metav1" is conventional shorthand for this package.
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	// "kubernetes" is the client library for talking to the Kubernetes API.
	"k8s.io/client-go/kubernetes"

	// "clientcmd" reads kubeconfig files and builds a connection config from them.
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	// Define a command-line flag called --kubeconfig.
	// When left empty, the tool assumes it's running inside the cluster
	// and uses the in-cluster service-account credentials automatically.
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig, leave empty for in-cluster")

	// Define a command-line flag called --address.
	// This is the host:port the HTTP server will listen on.
	listenAddr := flag.String("address", ":8080", "HTTP server listen address")

	// Actually parse the command-line flags provided by the user.
	flag.Parse()

	// Build a Kubernetes REST config from the kubeconfig file path.
	// This config contains the cluster URL, credentials, and TLS settings.
	kConfig, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		// panic stops the program immediately and prints the error.
		// For startup errors, this is acceptable — there's nothing to clean up.
		panic(err)
	}

	// Create a clientset using the config above.
	// A clientset is the main object we use to talk to Kubernetes.
	// It has methods for every Kubernetes resource type (Deployments, Pods, etc.).
	clientset, err := kubernetes.NewForConfig(kConfig)
	if err != nil {
		panic(err)
	}

	// Try fetching the Kubernetes server version.
	// This also acts as a connectivity check — if the cluster is unreachable,
	// we fail fast here rather than silently starting a broken server.
	version, err := getKubernetesVersion(clientset)
	if err != nil {
		panic(err)
	}

	// Print a confirmation message so the operator knows which cluster we connected to.
	fmt.Printf("Connected to Kubernetes %s\n", version)

	// Start the HTTP server. This call blocks until the server stops or crashes.
	// We pass the clientset so the HTTP handlers can use it to call the Kubernetes API.
	if err := startServer(*listenAddr, clientset); err != nil {
		panic(err)
	}
}

// getKubernetesVersion returns a string GitVersion of the Kubernetes server defined by the clientset.
//
// It calls the Kubernetes Discovery API, which returns metadata about the cluster
// including its version. If the cluster is unreachable, this returns an error,
// which makes it useful to check connectivity on startup.
func getKubernetesVersion(clientset kubernetes.Interface) (string, error) {
	// ServerVersion() calls the /version endpoint on the Kubernetes API server.
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		// Return an empty string and the error so the caller can decide what to do.
		return "", err
	}
	// version.String() returns something like "v1.26.3".
	return version.String(), nil
}

// startServer registers all HTTP route handlers and starts listening for requests.
// It blocks until the server shuts down or encounters a fatal error.
//
// listenAddr is a host:port string, e.g. ":8080" means "listen on all interfaces, port 8080".
// clientset is passed in so HTTP handlers can call the Kubernetes API.
func startServer(listenAddr string, clientset kubernetes.Interface) error {
	// Register the /healthz route. When a GET request arrives at /healthz,
	// Go calls the healthHandler function.
	http.HandleFunc("/healthz", healthHandler)

	// Register the /deployments/health route.
	// deploymentsHealthHandler is a function that *returns* a handler function,
	// so we call it here (with the clientset) to get the actual handler.
	http.HandleFunc("/deployments/health", deploymentsHealthHandler(clientset))

	fmt.Printf("Server listening on %s\n", listenAddr)

	// ListenAndServe starts the HTTP server. It only returns if something goes wrong.
	return http.ListenAndServe(listenAddr, nil)
}

// healthHandler responds with the health status of the application.
// It is used by load balancers and orchestration systems to check if this
// process is alive and accepting traffic. A 200 response means "I'm up".
func healthHandler(w http.ResponseWriter, r *http.Request) {
	// Write HTTP status 200 OK to the response.
	w.WriteHeader(http.StatusOK)

	// Write the response body. w.Write returns the number of bytes written and
	// an error — we only care about the error here.
	_, err := w.Write([]byte("ok"))
	if err != nil {
		// We can't return an error to the caller from a handler, so we log it.
		fmt.Println("failed writing to response")
	}
}

// DeploymentStatus holds the health information for a single Kubernetes Deployment.
//
// The `json:"..."` tags tell the encoding/json package what key names to use
// when serialising this struct to JSON. Without them, Go would use the field
// names directly (e.g. "DesiredReplicas" instead of "desiredReplicas").
type DeploymentStatus struct {
	// Name is the name of the Deployment as it appears in Kubernetes.
	Name string `json:"name"`

	// Namespace is the Kubernetes namespace the Deployment lives in.
	// Namespaces are used to group and isolate resources within a cluster.
	Namespace string `json:"namespace"`

	// DesiredReplicas is the number of pod replicas requested in the Deployment spec.
	// This is what the operator *asked for*.
	DesiredReplicas int32 `json:"desiredReplicas"`

	// ReadyReplicas is the number of pods that are currently passing their readiness checks.
	// This is what is *actually running and healthy*.
	ReadyReplicas int32 `json:"readyReplicas"`

	// Healthy is true only when ReadyReplicas equals DesiredReplicas AND DesiredReplicas > 0.
	// A deployment scaled to zero is treated as unhealthy because no pods are serving traffic.
	Healthy bool `json:"healthy"`
}

// DeploymentsHealthReport is the top-level JSON object returned by GET /deployments/health.
// It contains a summary flag and the per-deployment breakdown.
type DeploymentsHealthReport struct {
	// Deployments is a list of health details for every Deployment found in the cluster,
	// across all namespaces.
	Deployments []DeploymentStatus `json:"deployments"`

	// AllHealthy is a convenience flag: true only when *every* deployment is healthy.
	// Callers can check this single field instead of looping through Deployments.
	AllHealthy bool `json:"allHealthy"`
}

// getDeploymentsHealth queries the Kubernetes API for all Deployments across every namespace
// and checks whether each one has the expected number of ready pods.
//
// ctx (context) carries a deadline and cancellation signal. Passing r.Context() from an
// HTTP request means that if the HTTP client disconnects, the Kubernetes API call is also
// cancelled automatically, freeing up resources.
//
// clientset is the Kubernetes API client used to list Deployments.
func getDeploymentsHealth(ctx context.Context, clientset kubernetes.Interface) (*DeploymentsHealthReport, error) {
	// List all Deployments in all namespaces.
	// metav1.NamespaceAll is an empty string "" which Kubernetes interprets as "all namespaces".
	// metav1.ListOptions{} means "no filters — give me everything".
	deploymentList, err := clientset.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		// Wrap the error with extra context so callers know where it came from.
		// The %w verb lets the error be unwrapped later with errors.Is / errors.As.
		return nil, fmt.Errorf("listing deployments: %w", err)
	}

	// Build the report struct.
	// We pre-allocate the Deployments slice with the right capacity to avoid
	// unnecessary memory re-allocations as we append to it.
	// AllHealthy starts as true and is flipped to false the moment we find a problem.
	report := &DeploymentsHealthReport{
		Deployments: make([]DeploymentStatus, 0, len(deploymentList.Items)),
		AllHealthy:  true,
	}

	// Loop over every Deployment returned by the API.
	for _, d := range deploymentList.Items {
		// d.Spec.Replicas is a pointer (*int32), not a plain int32.
		// Kubernetes uses pointers so that "0 replicas" and "not set" can be distinguished.
		// When it is nil (not set), the Kubernetes default is 1 replica.
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas // dereference the pointer to get the actual value
		}

		// d.Status.ReadyReplicas is the live count reported by Kubernetes.
		ready := d.Status.ReadyReplicas

		// A deployment is healthy only if it has at least one desired replica
		// AND all desired replicas are ready.
		healthy := desired > 0 && ready == desired

		// If even one deployment is unhealthy, the whole report is unhealthy.
		if !healthy {
			report.AllHealthy = false
		}

		// Append this deployment's status to the report.
		report.Deployments = append(report.Deployments, DeploymentStatus{
			Name:            d.Name,
			Namespace:       d.Namespace,
			DesiredReplicas: desired,
			ReadyReplicas:   ready,
			Healthy:         healthy,
		})
	}

	// Return a pointer to the report (and no error).
	return report, nil
}

// deploymentsHealthHandler is a *handler factory*: it accepts the clientset and
// returns a function that matches the http.HandlerFunc signature (w, r).
// This pattern is used when a handler needs access to something that is created
// once at startup (the clientset) but used on every request.
//
// Response codes:
//   - 200 OK                    – every deployment has all pods ready (or there are none)
//   - 503 Service Unavailable   – one or more deployments are degraded
//   - 500 Internal Server Error – the Kubernetes API could not be reached
func deploymentsHealthHandler(clientset kubernetes.Interface) http.HandlerFunc {
	// The function we return is the actual HTTP handler.
	// Go closures "capture" variables from the surrounding scope,
	// so this inner function can use `clientset` even after deploymentsHealthHandler returns.
	return func(w http.ResponseWriter, r *http.Request) {
		// r.Context() is the context tied to this specific HTTP request.
		// If the client drops the connection, this context is cancelled automatically.
		report, err := getDeploymentsHealth(r.Context(), clientset)
		if err != nil {
			// http.Error writes a plain-text error body and the given status code.
			http.Error(w, fmt.Sprintf("failed to retrieve deployments: %v", err), http.StatusInternalServerError)
			return // stop processing this request
		}

		// Tell the client the response body is JSON.
		w.Header().Set("Content-Type", "application/json")

		// Write the appropriate HTTP status code before writing the body.
		// (Headers must be written before the body in HTTP.)
		if report.AllHealthy {
			w.WriteHeader(http.StatusOK) // 200
		} else {
			w.WriteHeader(http.StatusServiceUnavailable) // 503
		}

		// Encode the report struct as JSON and write it directly to the response writer.
		// json.NewEncoder(w).Encode() is more efficient than json.Marshal + w.Write
		// because it streams directly without an intermediate buffer.
		if err := json.NewEncoder(w).Encode(report); err != nil {
			fmt.Println("failed writing deployments health response")
		}
	}
}
