package metrics

import (
	"context"
	"fmt"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

const (
	defaultStep = 5 * time.Minute

	// cpuUsageQuery computes per-container CPU usage in cores via the
	// standard cAdvisor counter, matching the pod's owning Deployment by
	// name prefix (pod=~"^<workload>-.*") which is how ReplicaSet-managed
	// pod names are generated.
	cpuUsageQuery = `rate(container_cpu_usage_seconds_total{namespace=%q,pod=~%q,container=%q}[5m])`

	// memUsageQuery reads the cAdvisor working-set-bytes gauge, which is
	// what the kubelet itself uses for OOM decisions (unlike RSS).
	memUsageQuery = `container_memory_working_set_bytes{namespace=%q,pod=~%q,container=%q}`
)

// PrometheusSource is a UsageSource backed by a real Prometheus (or
// Prometheus-API-compatible, e.g. Thanos/Mimir) server.
type PrometheusSource struct {
	api promv1.API
}

// NewPrometheusSource builds a PrometheusSource pointed at addr (e.g.
// "http://prometheus.monitoring.svc:9090"). It performs no network I/O
// itself; connectivity is only exercised on the first query.
func NewPrometheusSource(addr string) (*PrometheusSource, error) {
	client, err := promapi.NewClient(promapi.Config{Address: addr})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	return &PrometheusSource{api: promv1.NewAPI(client)}, nil
}

func podNameRegex(workload string) string {
	return fmt.Sprintf("^%s-.*", workload)
}

func (p *PrometheusSource) CPUMillicoreSamples(ctx context.Context, q Query) ([]float64, error) {
	promQL := fmt.Sprintf(cpuUsageQuery, q.Namespace, podNameRegex(q.Workload), q.Container)
	samplesInCores, err := p.queryRange(ctx, promQL, q)
	if err != nil {
		return nil, err
	}
	millicores := make([]float64, len(samplesInCores))
	for i, v := range samplesInCores {
		millicores[i] = v * 1000
	}
	return millicores, nil
}

func (p *PrometheusSource) MemoryByteSamples(ctx context.Context, q Query) ([]float64, error) {
	promQL := fmt.Sprintf(memUsageQuery, q.Namespace, podNameRegex(q.Workload), q.Container)
	return p.queryRange(ctx, promQL, q)
}

func (p *PrometheusSource) queryRange(ctx context.Context, promQL string, q Query) ([]float64, error) {
	step := q.Step
	if step <= 0 {
		step = defaultStep
	}
	now := time.Now()
	r := promv1.Range{
		Start: now.Add(-q.Window),
		End:   now,
		Step:  step,
	}

	result, warnings, err := p.api.QueryRange(ctx, promQL, r)
	if err != nil {
		return nil, fmt.Errorf("prometheus query_range %q: %w", promQL, err)
	}
	_ = warnings // surfaced via logs by the caller if desired

	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T for query %q", result, promQL)
	}
	if len(matrix) == 0 {
		return nil, &ErrNoData{Query: q}
	}

	// A container can be represented by multiple series across pod
	// restarts/rollouts; flatten and sort by time so the caller sees one
	// continuous usage history for the workload+container pair.
	var points []usagePoint
	for _, series := range matrix {
		for _, v := range series.Values {
			if isStaleOrNaN(float64(v.Value)) {
				continue
			}
			points = append(points, usagePoint{t: v.Timestamp.Time(), v: float64(v.Value)})
		}
	}
	if len(points) == 0 {
		return nil, &ErrNoData{Query: q}
	}

	sortPointsByTime(points)
	samples := make([]float64, len(points))
	for i, pt := range points {
		samples[i] = pt.v
	}
	return samples, nil
}

// usagePoint is a single (time, value) observation flattened out of a
// Prometheus range-query matrix.
type usagePoint struct {
	t time.Time
	v float64
}

func isStaleOrNaN(v float64) bool {
	return v != v // NaN check without importing math for one call site
}

func sortPointsByTime(points []usagePoint) {
	for i := 1; i < len(points); i++ {
		for j := i; j > 0 && points[j-1].t.After(points[j].t); j-- {
			points[j-1], points[j] = points[j], points[j-1]
		}
	}
}
