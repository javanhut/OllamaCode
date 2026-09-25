package tui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modelServer answers /api/tags with the given model names.
func modelServer(t *testing.T, names ...string) string {
	var b strings.Builder
	b.WriteString(`{"models":[`)
	for i, n := range names {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"name":"` + n + `"}`)
	}
	b.WriteString(`]}`)
	body := b.String()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	t.Cleanup(s.Close)
	return s.URL
}

// The user's setup: nothing on localhost, models on a remote endpoint.
func routeModel(t *testing.T) *Model {
	m := statusTestModel()
	m.cfg.Host = "http://127.0.0.1:1" // nothing listening
	m.cfg.Providers = map[string]providerConfig{"ollama": {BaseURL: modelServer(t, "ornith-1.5:35B", "ornith-1.5:9b"), Kind: "ollama"}}
	m.cfg.Routes = map[string]string{}
	return m
}

func runRouteCheck(m *Model, mode, spec string) {
	m.cfg.Routes[mode] = spec
	m.applyRouteCheck(m.routeCheckCmd(mode, spec)().(routeCheckMsg))
}

// "/route plan big", as the old /help text suggested: flagged, with the fix.
func TestRouteCheckFlagsUnknownModel(t *testing.T) {
	m := routeModel(t)
	runRouteCheck(m, "plan", "big")
	if !strings.Contains(m.toast, `routed to "big"`) || !strings.Contains(m.toast, "/route plan") {
		t.Fatalf("toast = %q", m.toast)
	}
}

// A bare name that exists on an endpoint gets the prefixed spec suggested.
func TestRouteCheckSuggestsEndpointSpec(t *testing.T) {
	m := routeModel(t)
	runRouteCheck(m, "plan", "ornith-1.5:35B")
	if !strings.Contains(m.toast, "Did you mean /route plan ollama:ornith-1.5:35B?") {
		t.Fatalf("toast = %q", m.toast)
	}
}

func TestRouteCheckAcceptsReachableModel(t *testing.T) {
	m := routeModel(t)
	runRouteCheck(m, "write", "ollama:ornith-1.5:9b")
	if m.toast != "" {
		t.Fatalf("a valid route was flagged: %q", m.toast)
	}
}

func TestRouteFailureHintNamesTheRoute(t *testing.T) {
	m := routeModel(t)
	m.cfg.Routes["plan"] = "big"
	m.mode = PlanMode
	m.host.SetURI(m.cfg.Host)
	hint := m.routeFailureHint(errors.New(`Post "http://localhost:11434/api/chat": dial tcp [::1]:11434: connect: connection refused`))
	if !strings.Contains(hint, `plan mode is routed to "big"`) || !strings.Contains(hint, "/route off") {
		t.Fatalf("hint = %q", hint)
	}
	if m.routeFailureHint(errors.New("unexpected status code: 500")) != "" {
		t.Fatal("a server error is not a routing problem")
	}
}

func TestSameModel(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"qwen3.8", "qwen3.8:latest", true}, {"Ornith-1.5:35B", "ornith-1.5:35b", true}, {"ornith:35B", "ornith-1.5:35B", false}} {
		if sameModel(c.a, c.b) != c.want {
			t.Errorf("sameModel(%q, %q) != %v", c.a, c.b, c.want)
		}
	}
}
