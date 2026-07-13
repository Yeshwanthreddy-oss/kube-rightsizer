package recommend

import (
	"errors"
	"math"
	"testing"

	rightsizerv1alpha1 "github.com/kushagrasikka/kube-rightsizer/api/v1alpha1"
)

func almostEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

func TestPercentile_KnownDistribution(t *testing.T) {
	// 0..100 in steps of 1 (101 samples), p95 by R-7 linear interpolation
	// should land very close to 95.
	samples := make([]float64, 101)
	for i := range samples {
		samples[i] = float64(i)
	}
	got := Percentile(samples, 95)
	if !almostEqual(got, 95, 0.001) {
		t.Fatalf("p95 of 0..100 = %v, want ~95", got)
	}

	if got := Percentile(samples, 50); !almostEqual(got, 50, 0.001) {
		t.Fatalf("p50 of 0..100 = %v, want ~50", got)
	}
	if got := Percentile(samples, 0); got != 0 {
		t.Fatalf("p0 = %v, want 0", got)
	}
	if got := Percentile(samples, 100); got != 100 {
		t.Fatalf("p100 = %v, want 100", got)
	}
}

func TestPercentile_UnsortedInputUnmutated(t *testing.T) {
	samples := []float64{5, 1, 4, 2, 3}
	original := append([]float64{}, samples...)
	_ = Percentile(samples, 50)
	for i := range samples {
		if samples[i] != original[i] {
			t.Fatalf("Percentile mutated input slice: got %v, want %v", samples, original)
		}
	}
}

func TestPercentile_SingleSample(t *testing.T) {
	if got := Percentile([]float64{42}, 95); got != 42 {
		t.Fatalf("single-sample p95 = %v, want 42", got)
	}
}

func TestPercentile_EmptySamples(t *testing.T) {
	if got := Percentile(nil, 95); got != 0 {
		t.Fatalf("empty p95 = %v, want 0", got)
	}
}

func TestPercentile_OutOfRangeClampedToEnds(t *testing.T) {
	samples := []float64{10, 20, 30}
	if got := Percentile(samples, -10); got != 10 {
		t.Fatalf("p-10 = %v, want clamped to min 10", got)
	}
	if got := Percentile(samples, 150); got != 30 {
		t.Fatalf("p150 = %v, want clamped to max 30", got)
	}
}

func TestRecommend_HeadroomAppliedOnTopOfPercentile(t *testing.T) {
	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = 100 // constant usage -> p95 == 100 regardless of interpolation
	}
	thresholds := rightsizerv1alpha1.ResourceThresholds{
		Percentile:      95,
		HeadroomPercent: 20,
		LimitMultiplier: "2",
	}
	rec, err := Recommend(samples, thresholds, 10, ResourceCPU)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(rec.PercentileValue, 100, 0.001) {
		t.Fatalf("PercentileValue = %v, want 100", rec.PercentileValue)
	}
	if !almostEqual(rec.Request, 120, 0.001) {
		t.Fatalf("Request = %v, want 120 (100 * 1.20)", rec.Request)
	}
	if !almostEqual(rec.Limit, 240, 0.001) {
		t.Fatalf("Limit = %v, want 240 (120 * 2)", rec.Limit)
	}
	if rec.ClampedLow || rec.ClampedHigh {
		t.Fatalf("expected no clamping, got low=%v high=%v", rec.ClampedLow, rec.ClampedHigh)
	}
}

func TestRecommend_DefaultsWhenThresholdsZero(t *testing.T) {
	samples := make([]float64, 50)
	for i := range samples {
		samples[i] = 50
	}
	// Zero-value ResourceThresholds should still produce a sane
	// recommendation using documented defaults (p95, 0% headroom, 2x limit).
	rec, err := Recommend(samples, rightsizerv1alpha1.ResourceThresholds{}, 10, ResourceCPU)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(rec.Request, 50, 0.001) {
		t.Fatalf("Request = %v, want 50", rec.Request)
	}
	if !almostEqual(rec.Limit, 100, 0.001) {
		t.Fatalf("Limit = %v, want 100", rec.Limit)
	}
}

func TestRecommend_InsufficientSamplesReturnsTypedError(t *testing.T) {
	samples := []float64{1, 2, 3}
	_, err := Recommend(samples, rightsizerv1alpha1.ResourceThresholds{}, 10, ResourceCPU)
	if err == nil {
		t.Fatal("expected error for insufficient samples, got nil")
	}
	var insufficient *ErrInsufficientSamples
	if !errors.As(err, &insufficient) {
		t.Fatalf("expected *ErrInsufficientSamples, got %T: %v", err, err)
	}
	if insufficient.Have != 3 || insufficient.Want != 10 {
		t.Fatalf("got Have=%d Want=%d, want Have=3 Want=10", insufficient.Have, insufficient.Want)
	}
}

func TestRecommend_MinRequestClampsUp(t *testing.T) {
	samples := make([]float64, 20)
	for i := range samples {
		samples[i] = 5 // millicores, tiny usage
	}
	thresholds := rightsizerv1alpha1.ResourceThresholds{
		Percentile:      95,
		HeadroomPercent: 10,
		MinRequest:      "50m", // 50 millicores floor
		LimitMultiplier: "2",
	}
	rec, err := Recommend(samples, thresholds, 10, ResourceCPU)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.ClampedLow {
		t.Fatalf("expected ClampedLow=true, got false (Request=%v)", rec.Request)
	}
	if !almostEqual(rec.Request, 50, 0.001) {
		t.Fatalf("Request = %v, want clamped to 50", rec.Request)
	}
}

func TestRecommend_MaxRequestClampsDown(t *testing.T) {
	samples := make([]float64, 20)
	for i := range samples {
		samples[i] = 5000 // 5 cores of usage
	}
	thresholds := rightsizerv1alpha1.ResourceThresholds{
		Percentile:      95,
		HeadroomPercent: 10,
		MaxRequest:      "2000m", // 2 core ceiling
		LimitMultiplier: "2",
	}
	rec, err := Recommend(samples, thresholds, 10, ResourceCPU)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.ClampedHigh {
		t.Fatalf("expected ClampedHigh=true, got false (Request=%v)", rec.Request)
	}
	if !almostEqual(rec.Request, 2000, 0.001) {
		t.Fatalf("Request = %v, want clamped to 2000", rec.Request)
	}
}

func TestRecommend_CustomLimitMultiplier(t *testing.T) {
	samples := make([]float64, 20)
	for i := range samples {
		samples[i] = 100
	}
	thresholds := rightsizerv1alpha1.ResourceThresholds{
		Percentile:      95,
		HeadroomPercent: 0,
		LimitMultiplier: "1.5",
	}
	rec, err := Recommend(samples, thresholds, 10, ResourceCPU)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(rec.Limit, 150, 0.001) {
		t.Fatalf("Limit = %v, want 150 (100 * 1.5)", rec.Limit)
	}
}

func TestRecommend_MemoryUnitsInBytes(t *testing.T) {
	// Simulate memory usage samples in bytes with a "Mi"-based clamp.
	samples := make([]float64, 30)
	for i := range samples {
		samples[i] = 100 * 1024 * 1024 // 100Mi constant usage
	}
	thresholds := rightsizerv1alpha1.ResourceThresholds{
		Percentile:      95,
		HeadroomPercent: 25,
		MinRequest:      "64Mi",
		LimitMultiplier: "1.5",
	}
	rec, err := Recommend(samples, thresholds, 10, ResourceMemory)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantRequest := 100 * 1024 * 1024 * 1.25
	if !almostEqual(rec.Request, wantRequest, 1) {
		t.Fatalf("Request = %v, want %v", rec.Request, wantRequest)
	}
	if rec.ClampedLow {
		t.Fatalf("did not expect clamping, request %v > floor 64Mi", rec.Request)
	}
}

func TestChangePercent(t *testing.T) {
	cases := []struct {
		name              string
		current, proposed float64
		want              float64
	}{
		{"no change", 100, 100, 0},
		{"50 percent increase", 100, 150, 50},
		{"50 percent decrease", 100, 50, 50},
		{"from zero to positive is 100pct", 0, 10, 100},
		{"zero to zero is zero", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ChangePercent(c.current, c.proposed)
			if !almostEqual(got, c.want, 0.001) {
				t.Fatalf("ChangePercent(%v, %v) = %v, want %v", c.current, c.proposed, got, c.want)
			}
		})
	}
}

func TestRecommend_HighVarianceRealisticWorkload(t *testing.T) {
	// Simulate a bursty workload: mostly idle around 50m with periodic
	// spikes to 400m. p95 should sit near the spike range, not the idle
	// baseline, demonstrating the engine resists being dragged down by a
	// noisy-but-mostly-idle signal.
	samples := make([]float64, 0, 200)
	for i := 0; i < 180; i++ {
		samples = append(samples, 50)
	}
	for i := 0; i < 20; i++ {
		samples = append(samples, 400)
	}
	thresholds := rightsizerv1alpha1.ResourceThresholds{
		Percentile:      95,
		HeadroomPercent: 15,
		LimitMultiplier: "2",
	}
	rec, err := Recommend(samples, thresholds, 10, ResourceCPU)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.PercentileValue < 50 {
		t.Fatalf("PercentileValue = %v, expected it to reflect the spike tail, not the idle baseline", rec.PercentileValue)
	}
}
