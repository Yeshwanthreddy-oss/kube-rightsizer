package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPodNameRegex(t *testing.T) {
	got := podNameRegex("checkout-api")
	want := "^checkout-api-.*"
	if got != want {
		t.Fatalf("podNameRegex = %q, want %q", got, want)
	}
}

func TestIsStaleOrNaN(t *testing.T) {
	if isStaleOrNaN(1.0) {
		t.Fatal("1.0 should not be flagged as NaN")
	}
	nan := 0.0
	nan = nan / nan // produces NaN without importing math
	if !isStaleOrNaN(nan) {
		t.Fatal("NaN should be flagged")
	}
}

func TestSortPointsByTime(t *testing.T) {
	base := time.Now()
	points := []usagePoint{
		{t: base.Add(3 * time.Minute), v: 3},
		{t: base, v: 1},
		{t: base.Add(1 * time.Minute), v: 2},
	}
	sortPointsByTime(points)
	for i := 1; i < len(points); i++ {
		if points[i].t.Before(points[i-1].t) {
			t.Fatalf("points not sorted: %+v", points)
		}
	}
	if points[0].v != 1 || points[2].v != 3 {
		t.Fatalf("unexpected order after sort: %+v", points)
	}
}

// fakePrometheusHTTP builds an httptest.Server that responds to
// /api/v1/query_range like a real Prometheus, returning valuesPerSeries
// verbatim. This exercises PrometheusSource's HTTP + PromQL + parsing path
// end-to-end without a real Prometheus or network access (httptest binds to
// loopback only).
func fakePrometheusHTTP(t *testing.T, values [][2]interface{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var vals string
		for i, v := range values {
			if i > 0 {
				vals += ","
			}
			vals += fmt.Sprintf(`[%v,"%v"]`, v[0], v[1])
		}
		body := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"container":"app"},"values":[%s]}]}}`, vals)
		_, _ = w.Write([]byte(body))
	})
	return httptest.NewServer(mux)
}

func TestPrometheusSource_CPUMillicoreSamples(t *testing.T) {
	now := time.Now().Unix()
	srv := fakePrometheusHTTP(t, [][2]interface{}{
		{now - 600, "0.1"}, // 0.1 core -> 100 millicores
		{now - 300, "0.2"}, // 200 millicores
		{now, "0.15"},      // 150 millicores
	})
	defer srv.Close()

	src, err := NewPrometheusSource(srv.URL)
	if err != nil {
		t.Fatalf("NewPrometheusSource: %v", err)
	}

	samples, err := src.CPUMillicoreSamples(context.Background(), Query{
		Namespace: "default", Workload: "checkout-api", Container: "app", Window: time.Hour,
	})
	if err != nil {
		t.Fatalf("CPUMillicoreSamples: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("got %d samples, want 3: %v", len(samples), samples)
	}
	// Values should be scaled from cores to millicores (x1000).
	want := []float64{100, 200, 150}
	for i, w := range want {
		if diff := samples[i] - w; diff > 0.001 || diff < -0.001 {
			t.Fatalf("sample[%d] = %v, want %v (full: %v)", i, samples[i], w, samples)
		}
	}
}

func TestPrometheusSource_MemoryByteSamples(t *testing.T) {
	now := time.Now().Unix()
	srv := fakePrometheusHTTP(t, [][2]interface{}{
		{now - 300, "104857600"}, // 100Mi
		{now, "125829120"},       // 120Mi
	})
	defer srv.Close()

	src, err := NewPrometheusSource(srv.URL)
	if err != nil {
		t.Fatalf("NewPrometheusSource: %v", err)
	}

	samples, err := src.MemoryByteSamples(context.Background(), Query{
		Namespace: "default", Workload: "checkout-api", Container: "app", Window: time.Hour,
	})
	if err != nil {
		t.Fatalf("MemoryByteSamples: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(samples))
	}
	if samples[0] != 104857600 || samples[1] != 125829120 {
		t.Fatalf("unexpected memory samples: %v", samples)
	}
}

func TestPrometheusSource_NoData(t *testing.T) {
	// A handler that returns an empty matrix, as Prometheus does when no
	// series match the query.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src, err := NewPrometheusSource(srv.URL)
	if err != nil {
		t.Fatalf("NewPrometheusSource: %v", err)
	}
	_, err = src.CPUMillicoreSamples(context.Background(), Query{
		Namespace: "default", Workload: "ghost-app", Container: "app", Window: time.Hour,
	})
	if err == nil {
		t.Fatal("expected ErrNoData, got nil")
	}
	var noData *ErrNoData
	if !isErrNoData(err, &noData) {
		t.Fatalf("expected *ErrNoData, got %T: %v", err, err)
	}
}

// isErrNoData is a small errors.As wrapper kept local to avoid importing
// errors just for this one assertion in multiple test files.
func isErrNoData(err error, target **ErrNoData) bool {
	e, ok := err.(*ErrNoData)
	if ok {
		*target = e
	}
	return ok
}

func TestFakeSource_CPUAndMemory(t *testing.T) {
	f := NewFakeSource()
	f.SetCPU("ns", "wl", "c", []float64{1, 2, 3})
	f.SetMemory("ns", "wl", "c", []float64{10, 20})

	cpu, err := f.CPUMillicoreSamples(context.Background(), Query{Namespace: "ns", Workload: "wl", Container: "c"})
	if err != nil || len(cpu) != 3 {
		t.Fatalf("CPU: got %v, err %v", cpu, err)
	}
	mem, err := f.MemoryByteSamples(context.Background(), Query{Namespace: "ns", Workload: "wl", Container: "c"})
	if err != nil || len(mem) != 2 {
		t.Fatalf("Mem: got %v, err %v", mem, err)
	}
}

func TestFakeSource_NoDataForUnknownKey(t *testing.T) {
	f := NewFakeSource()
	_, err := f.CPUMillicoreSamples(context.Background(), Query{Namespace: "ns", Workload: "missing", Container: "c"})
	if err == nil {
		t.Fatal("expected ErrNoData for unregistered key")
	}
}

func TestFakeSource_ErrOverride(t *testing.T) {
	f := NewFakeSource()
	f.SetCPU("ns", "wl", "c", []float64{1})
	f.Err = fmt.Errorf("boom")
	_, err := f.CPUMillicoreSamples(context.Background(), Query{Namespace: "ns", Workload: "wl", Container: "c"})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected override error 'boom', got %v", err)
	}
}
