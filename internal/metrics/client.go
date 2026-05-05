// Package metrics defines the abstraction the controller uses to pull
// historical container CPU/memory usage samples out of Prometheus (fed by
// metrics-server / kube-state-metrics style exporters, or any Prometheus
// compatible source with the standard cAdvisor container metric names).
//
// The controller only ever talks to the UsageSource interface, never to a
// concrete Prometheus client directly, so unit tests can substitute a fake
// in-memory source and CI never needs a live Prometheus.
package metrics

import (
	"context"
	"fmt"
	"time"
)

// Query identifies the container whose historical usage is requested.
type Query struct {
	Namespace string
	Workload  string // Deployment name
	Container string
	Window    time.Duration
	// Step is the sampling resolution requested from Prometheus. Defaults
	// applied by implementations if zero.
	Step time.Duration
}

// UsageSource returns historical CPU (millicores) and memory (bytes) usage
// samples for a container over a rolling window.
type UsageSource interface {
	// CPUMillicoreSamples returns one sample per Step across Window,
	// oldest first, in millicores.
	CPUMillicoreSamples(ctx context.Context, q Query) ([]float64, error)
	// MemoryByteSamples returns one sample per Step across Window, oldest
	// first, in bytes.
	MemoryByteSamples(ctx context.Context, q Query) ([]float64, error)
}

// ErrNoData indicates Prometheus had no series matching the query, as
// distinct from a transport/query error.
type ErrNoData struct {
	Query Query
}

func (e *ErrNoData) Error() string {
	return fmt.Sprintf("no usage data for %s/%s container %q over %s", e.Query.Namespace, e.Query.Workload, e.Query.Container, e.Query.Window)
}
