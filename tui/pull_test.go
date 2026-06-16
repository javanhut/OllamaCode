package tui

import (
	"strings"
	"testing"
)

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
		{3 * 1024 * 1024 * 1024, "3.0 GB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestRenderProgressBar(t *testing.T) {
	// Half-complete bar should read 50% and contain both fill and empty runes.
	out := renderProgressBar(500, 1000, 22)
	if !strings.Contains(out, "50%") {
		t.Errorf("expected 50%% in %q", out)
	}
	if !strings.Contains(out, "#") || !strings.Contains(out, "-") {
		t.Errorf("expected a partially filled bar, got %q", out)
	}

	// Zero total must not divide-by-zero or exceed bounds.
	if out := renderProgressBar(0, 0, 22); !strings.Contains(out, "0%") {
		t.Errorf("expected 0%% for empty total, got %q", out)
	}

	// Over-complete clamps to 100%.
	if out := renderProgressBar(2000, 1000, 22); !strings.Contains(out, "100%") {
		t.Errorf("expected 100%% when completed exceeds total, got %q", out)
	}
}
