package tui

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
)

// Route checking. /route used to save any name it was given, and a bare name
// runs on the default host, so "/route plan big" (the words /help used to
// suggest) bound plan mode to a model that existed nowhere, on a localhost
// daemon that wasn't running. Nothing said so until the mode switched and every
// request failed with "connection refused".

// routeCheckMsg reports whether a route's model exists on its host.
type routeCheckMsg struct {
	mode, spec string
	host       string   // URL the spec resolves to
	found      bool     // the model is listed there
	err        error    // the host could not be asked
	suggest    []string // specs on configured hosts whose model matches
}

// routeCheckCmd asks the host a route resolves to whether it has the model,
// and looks for the same model on every configured host to suggest instead.
func (m *Model) routeCheckCmd(mode, spec string) tea.Cmd {
	cfg := m.cfg
	return func() tea.Msg {
		host, model := hostForSpec(cfg, spec)
		msg := routeCheckMsg{mode: mode, spec: spec, host: host.URL()}
		if list, err := host.GetModelList(); err != nil {
			msg.err = err
		} else {
			msg.found = hasModel(list.Models, model)
		}
		if msg.found {
			return msg
		}
		// Where else does this model live? The default host, then each
		// endpoint, in a stable order.
		candidates := []string{""}
		for name := range cfg.Providers {
			candidates = append(candidates, name)
		}
		sort.Strings(candidates[1:])
		for _, provider := range candidates {
			h := defaultHostFor(cfg)
			prefix := ""
			if provider != "" {
				h, prefix = providerHostFor(cfg, provider), provider+":"
			}
			if h.URL() == msg.host {
				continue // already asked
			}
			if list, err := h.GetModelList(); err == nil {
				for _, s := range list.Models {
					if sameModel(s.Name, model) {
						msg.suggest = append(msg.suggest, prefix+s.Name)
					}
				}
			}
		}
		return msg
	}
}

// hasModel reports whether name is among the listed models.
func hasModel(models []api.ModelSummary, name string) bool {
	return slices.ContainsFunc(models, func(s api.ModelSummary) bool { return sameModel(s.Name, name) })
}

// sameModel compares model names the way Ollama resolves them: a bare name
// means its ":latest" tag, and case does not matter.
func sameModel(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if !strings.Contains(s[strings.LastIndex(s, "/")+1:], ":") {
			s += ":latest"
		}
		return s
	}
	return norm(a) == norm(b)
}

// applyRouteCheck warns about a route whose model cannot be reached.
func (m *Model) applyRouteCheck(msg routeCheckMsg) {
	if msg.found || strings.TrimSpace(m.cfg.Routes[msg.mode]) != msg.spec {
		return // fine, or the route changed since the check started
	}
	why := fmt.Sprintf("no model %q there", msg.spec)
	if msg.err != nil {
		why = "the host is unreachable"
	}
	warn := fmt.Sprintf("%s mode is routed to %q on %s, but %s.", msg.mode, msg.spec, msg.host, why)
	switch len(msg.suggest) {
	case 0:
		warn += fmt.Sprintf(" Fix it with /route %s <endpoint>:<model>, or /route off.", msg.mode)
	default:
		warn += fmt.Sprintf(" Did you mean /route %s %s?", msg.mode, msg.suggest[0])
	}
	// Several routes can be checked at once; keep every warning visible.
	if strings.Contains(m.toast, "mode is routed to") {
		warn = m.toast + " · " + warn
	}
	m.toast = warn
	m.noteActivity(warn)
}

// routeCheckAllCmd checks every configured route, so a broken one is flagged
// when ocode starts rather than when its mode is first entered.
func (m *Model) routeCheckAllCmd() tea.Cmd {
	var cmds []tea.Cmd
	for mode, spec := range m.cfg.Routes {
		if spec = strings.TrimSpace(spec); spec != "" {
			cmds = append(cmds, m.routeCheckCmd(mode, spec))
		}
	}
	return tea.Batch(cmds...)
}

// routeFailureHint explains a connection failure on a routed model: the error
// alone names a host, not the route that sent the request there.
func (m *Model) routeFailureHint(err error) string {
	spec := strings.TrimSpace(m.cfg.Routes[m.mode.String()])
	if spec == "" || err == nil {
		return ""
	}
	text := err.Error()
	if !strings.Contains(text, "connection refused") && !strings.Contains(text, "no such host") &&
		!strings.Contains(text, "dial tcp") && !strings.Contains(text, "i/o timeout") {
		return ""
	}
	return fmt.Sprintf("\n%s mode is routed to %q on %s. Point it at a reachable model with /route %s <endpoint>:<model>, or turn routing off with /route off.",
		m.mode, spec, m.host.URL(), m.mode)
}
