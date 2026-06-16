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

func TestPullErrorHint(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		err       string
		wantHas   string // substring the hint must contain
		wantEmpty bool
	}{
		{"bad tag local", "http://localhost:11434", "pull model manifest: file does not exist", "-cloud suffix", false},
		{"bad tag on cloud host", "https://ollama.com", "pull model manifest: file does not exist", "local daemon", false},
		{"unauthorized", "https://ollama.com", "unauthorized", "ollama signin", false},
		{"unknown error", "http://localhost:11434", "connection refused", "", true},
	}
	for _, c := range cases {
		got := pullErrorHint(c.host, c.err)
		if c.wantEmpty {
			if got != "" {
				t.Errorf("%s: expected no hint, got %q", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.wantHas) {
			t.Errorf("%s: hint %q does not contain %q", c.name, got, c.wantHas)
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
