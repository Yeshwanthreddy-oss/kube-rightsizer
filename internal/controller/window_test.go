package controller

import (
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"0.5d", 12 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"", 0, true},
		{"notaduration", 0, true},
		{"7x", 0, true},
	}
	for _, c := range cases {
		got, err := ParseWindow(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseWindow(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseWindow(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseWindow(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
