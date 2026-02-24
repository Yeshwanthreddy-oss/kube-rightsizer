package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseWindow parses a duration string that additionally accepts a "d" (day)
// suffix on top of everything time.ParseDuration already understands, since
// "7d" is the natural way to write a rolling week in a CRD spec and
// time.ParseDuration alone rejects it.
func ParseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty window")
	}
	if strings.HasSuffix(s, "d") {
		numPart := strings.TrimSuffix(s, "d")
		days, err := strconv.ParseFloat(numPart, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid day-based window %q: %w", s, err)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid window %q: %w", s, err)
	}
	return d, nil
}
