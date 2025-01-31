package metrics

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"

	"k8s.io/client-go/rest"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	metricKubeClientRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_kube_client_requests_total",
		Help: "Counter incremented for each kube client request.",
	},
		[]string{"controller", "method", "resource"},
	)
)

func init() {
	metrics.Registry.MustRegister(metricKubeClientRequests)
}

// NewClientWithMetricsOrDie creates a new controller-runtime client with a wrapper which increments
// metrics for requests by controller name, HTTP method, and URL path. The client will re-use the
// managers cache. This should be used in all Hive controllers.
func NewClientWithMetricsOrDie(mgr manager.Manager, ctrlrName string) client.Client {
	// Copy the rest config as we want our round trippers to be controller specific.
	cfg := rest.CopyConfig(mgr.GetConfig())
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return &ControllerMetricsTripper{
			RoundTripper: rt,
			controller:   ctrlrName,
		}
	}

	options := client.Options{
		Scheme: mgr.GetScheme(),
		Mapper: mgr.GetRESTMapper(),
	}
	c, err := client.New(cfg, options)
	if err != nil {
		log.WithError(err).Fatal("unable to initialize metrics wrapped client")
	}

	return &client.DelegatingClient{
		Reader: &client.DelegatingReader{
			CacheReader:  mgr.GetCache(),
			ClientReader: c,
		},
		Writer:       c,
		StatusClient: c,
	}
}

// ControllerMetricsTripper is a RoundTripper implementation which tracks our metrics for client requests.
type ControllerMetricsTripper struct {
	http.RoundTripper
	controller string
}

// RoundTrip implements the http RoundTripper interface.
func (cmt *ControllerMetricsTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	metricKubeClientRequests.WithLabelValues(cmt.controller, req.Method, parsePath(req.URL.Path)).Inc()
	// Call the nested RoundTripper.
	resp, err := cmt.RoundTripper.RoundTrip(req)
	return resp, err
}

// parsePath returns a group/version/resource string from the given path. Used to avoid per cluster metrics
// for cardinality reasons.
func parsePath(path string) string {
	tokens := strings.Split(path[1:], "/")
	fmt.Printf("tokens: %v\n", tokens)
	if tokens[0] == "api" {
		// Handle core resources:
		if len(tokens) == 3 || len(tokens) == 4 {
			return strings.Join([]string{"core", tokens[1], tokens[2]}, "/")
		}
		// Handle operators on direct namespaced resources:
		if len(tokens) > 4 && tokens[2] == "namespaces" {
			return strings.Join([]string{"core", tokens[1], tokens[4]}, "/")
		}
	} else if tokens[0] == "apis" {
		// Handle resources with apigroups:
		if len(tokens) == 4 || len(tokens) == 5 {
			return strings.Join([]string{tokens[1], tokens[2], tokens[3]}, "/")
		}
		if len(tokens) > 5 && tokens[3] == "namespaces" {
			return strings.Join([]string{tokens[1], tokens[2], tokens[5]}, "/")
		}

	}
	log.Warnf("unable to parse path for client metrics: %s", path)

	return "unknown-resource"
}
