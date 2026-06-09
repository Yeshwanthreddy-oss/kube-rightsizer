// Package recommend implements the p95-based resource recommendation math
// that turns a window of raw Prometheus usage samples into a recommended
// Kubernetes request/limit pair. It has no dependency on the Kubernetes API
// or Prometheus client so it can be tested purely with in-memory slices.
package recommend

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	rightsizerv1alpha1 "github.com/kushagrasikka/kube-rightsizer/api/v1alpha1"
)

// ErrInsufficientSamples is returned when a container does not have enough
// history to safely recommend a value.
type ErrInsufficientSamples struct {
	Have, Want int
}

func (e *ErrInsufficientSamples) Error() string {
	return fmt.Sprintf("insufficient samples: have %d, want at least %d", e.Have, e.Want)
}

// ResourceKind selects the unit convention used when parsing quantity
// strings (MinRequest/MaxRequest) so they land in the same base unit as the
// sample data: millicores for CPU, bytes for memory. This mirrors how a
// caller would read current values off a live container with
// c.Resources.Requests.Cpu().MilliValue() and
// c.Resources.Requests.Memory().Value().
type ResourceKind string

const (
	ResourceCPU    ResourceKind = "cpu"
	ResourceMemory ResourceKind = "memory"
)

// Recommendation is the computed request/limit pair for one resource
// dimension (cpu or memory), all values in the resource's base unit
// (millicores for CPU, bytes for memory).
type Recommendation struct {
	// PercentileValue is the raw p-th percentile of the sample window
	// before headroom or clamping is applied.
	PercentileValue float64
	// Request is PercentileValue with headroom applied and clamped to
	// [MinRequest, MaxRequest].
	Request float64
	// Limit is Request scaled by LimitMultiplier.
	Limit float64
	// ClampedLow/ClampedHigh record whether MinRequest/MaxRequest altered
	// the recommendation, for observability.
	ClampedLow  bool
	ClampedHigh bool
}

// Percentile returns the p-th percentile (0-100, linear interpolation
// between closest ranks) of samples. samples need not be sorted; the slice
// passed in is not mutated. Panics if p is outside [0,100] or samples is
// empty; callers must validate sample count first via MinSamples.
func Percentile(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	if len(samples) == 1 {
		return samples[0]
	}
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)

	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}

	// Linear interpolation on the "R-7" method (same as numpy's default),
	// which is a common, well-understood definition.
	rank := (p / 100) * float64(len(sorted)-1)
	lowIdx := int(math.Floor(rank))
	highIdx := int(math.Ceil(rank))
	if lowIdx == highIdx {
		return sorted[lowIdx]
	}
	frac := rank - float64(lowIdx)
	return sorted[lowIdx]*(1-frac) + sorted[highIdx]*frac
}

// Recommend computes a Recommendation for one resource dimension from a set
// of usage samples (already expressed in the resource's base unit) and the
// ResourceThresholds configured on a ResourcePolicy.
//
// minSamples is enforced by the caller (typically ResourcePolicySpec.MinSamples)
// so the same helper serves both CPU and memory without duplicating the
// check.
func Recommend(samples []float64, thresholds rightsizerv1alpha1.ResourceThresholds, minSamples int, kind ResourceKind) (*Recommendation, error) {
	if len(samples) < minSamples {
		return nil, &ErrInsufficientSamples{Have: len(samples), Want: minSamples}
	}

	percentile := thresholds.Percentile
	if percentile <= 0 {
		percentile = 95
	}
	headroom := float64(thresholds.HeadroomPercent) / 100.0

	rawP := Percentile(samples, float64(percentile))
	request := rawP * (1 + headroom)

	clampedLow, clampedHigh := false, false
	if thresholds.MinRequest != "" {
		if min, err := parseQuantityFloat(thresholds.MinRequest, kind); err == nil && request < min {
			request = min
			clampedLow = true
		}
	}
	if thresholds.MaxRequest != "" {
		if max, err := parseQuantityFloat(thresholds.MaxRequest, kind); err == nil && request > max {
			request = max
			clampedHigh = true
		}
	}

	multiplier := 2.0
	if thresholds.LimitMultiplier != "" {
		if m, err := strconv.ParseFloat(strings.TrimSpace(thresholds.LimitMultiplier), 64); err == nil && m > 0 {
			multiplier = m
		}
	}

	return &Recommendation{
		PercentileValue: rawP,
		Request:         request,
		Limit:           request * multiplier,
		ClampedLow:      clampedLow,
		ClampedHigh:     clampedHigh,
	}, nil
}

// parseQuantityFloat parses a Kubernetes resource.Quantity string (e.g.
// "250m", "512Mi") into its float64 value in the base unit matching kind:
// millicores for ResourceCPU, bytes for ResourceMemory. This must stay in
// lockstep with however the caller expressed its usage samples.
func parseQuantityFloat(s string, kind ResourceKind) (float64, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, err
	}
	if kind == ResourceCPU {
		return float64(q.MilliValue()), nil
	}
	return q.AsApproximateFloat64(), nil
}

// ChangePercent returns the absolute relative change from current to
// recommended, as a percentage (e.g. 25.0 for a 25% change). If current is
// zero, any positive recommendation is treated as a 100%+ change so it is
// never silently ignored by a change threshold.
func ChangePercent(current, recommended float64) float64 {
	if current <= 0 {
		if recommended <= 0 {
			return 0
		}
		return 100
	}
	return math.Abs(recommended-current) / current * 100
}
