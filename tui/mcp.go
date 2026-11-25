package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/tools"
)

// mcpServerConfig describes one external MCP server. Exactly one of command
// (stdio subprocess) or url (Streamable HTTP endpoint) must be set; args,
// work_dir and env_allow only apply to stdio, headers and headers_env only
// to HTTP. Every other guardrail applies to both transports identically.
type mcpServerConfig struct {
	Command         string            `json:"command,omitempty"`
	URL             string            `json:"url,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	HeadersEnv      map[string]string `json:"headers_env,omitempty"`
	Args            []string          `json:"args,omitempty"`
	ProtocolVersion string            `json:"protocol_version,omitempty"`
	ReadOnly        bool              `json:"read_only,omitempty"`
	SmallModelSafe  bool              `json:"small_model_safe,omitempty"`
	Disabled        bool              `json:"disabled,omitempty"`
	Trusted         bool              `json:"trusted,omitempty"`
	WorkDir         string            `json:"work_dir,omitempty"`
	EnvAllow        []string          `json:"env_allow,omitempty"`
	CallTimeoutSec  int               `json:"call_timeout_sec,omitempty"`
	MaxResponseKB   int               `json:"max_response_kb,omitempty"`
}

func connectMCPServers(configs map[string]mcpServerConfig, registry *tools.Registry, pins *mcpPinStore) ([]tools.MCPServer, []string) {
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	var servers []tools.MCPServer
	var warnings []string
	for _, name := range names {
		cfg := configs[name]
		if cfg.Disabled {
			continue
		}
		if !cfg.Trusted {
			warnings = append(warnings, fmt.Sprintf("MCP %s: disabled until trusted=true is explicitly configured", name))
			continue
		}
		maxBytes := cfg.MaxResponseKB * 1024
		server, err := tools.NewMCPServerFromSpec(tools.ExternalServerSpec{
			Name: name, Command: cfg.Command, Args: cfg.Args, WorkDir: cfg.WorkDir,
			EnvAllow: cfg.EnvAllow, URL: cfg.URL, Headers: cfg.Headers, HeadersEnv: cfg.HeadersEnv,
			MaxResponseBytes: maxBytes,
			CallTimeout:      time.Duration(cfg.CallTimeoutSec) * time.Second,
		})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("MCP %s: %v", name, err))
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err = server.Initialize(ctx, cfg.ProtocolVersion)
		if err == nil {
			policy := tools.ToolPolicy{
				Modes: tools.ModeMutable, SmallModelSafe: cfg.SmallModelSafe,
				Destructive: true, Network: true, Cost: tools.ToolCostHigh,
			}
			if cfg.ReadOnly {
				policy.Modes = tools.ModeReadOnly
				policy.Destructive = false
			}
			var definitions []tools.Tool
			definitions, err = server.ListTools(ctx, policy)
			if err == nil {
				for _, definition := range definitions {
					if _, exists := registry.Lookup(definition.Function.Name); exists {
						err = fmt.Errorf("tool name collision: %s", definition.Function.Name)
						break
					}
				}
			}
			if err == nil {
				var pinned bool
				if pinned, err = checkMCPPin(pins, name, definitions); err == nil && !pinned {
					err = fmt.Errorf("tool definitions changed since this server was approved — its tools are disabled. Review the server, then run /mcp repin %s", name)
				}
			}
			if err == nil {
				err = registry.ReplacePrefix(server.Namespace(), definitions)
				serverName := name
				server.SetToolsChangedHandler(func() {
					refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer refreshCancel()
					updated, refreshErr := server.ListTools(refreshCtx, policy)
					if refreshErr != nil {
						return
					}
					// A live list_changed is the rug pull's natural delivery
					// route: re-check the pin and drop every tool on mismatch.
					if ok, _ := checkMCPPin(pins, serverName, updated); !ok {
						_ = registry.ReplacePrefix(server.Namespace(), nil)
						return
					}
					_ = registry.ReplacePrefix(server.Namespace(), updated)
				})
			}
		}
		cancel()
		if err != nil {
			_ = server.Close()
			warnings = append(warnings, fmt.Sprintf("MCP %s: %v", name, err))
			continue
		}
		servers = append(servers, server)
		go func(server tools.MCPServer) {
			<-server.Done()
			_ = registry.ReplacePrefix(server.Namespace(), nil)
		}(server)
	}
	return servers, compactWarnings(warnings)
}

func compactWarnings(warnings []string) []string {
	for i, warning := range warnings {
		warnings[i] = strings.TrimSpace(warning)
	}
	return warnings
}

// mcpCommand handles "/mcp" (list configured servers and pin state) and
// "/mcp repin <name>" (forget a server's pin so its current tool definitions
// are approved on the next connection).
func (m *Model) mcpCommand(arg string) string {
	pins := m.mcpPins
	if pins == nil {
		pins = loadMCPPins(mcpPinsPath())
	}
	if name, ok := strings.CutPrefix(arg, "repin"); ok {
		name = strings.TrimSpace(name)
		if _, exists := m.cfg.MCPServers[name]; !exists {
			return fmt.Sprintf("No MCP server named %q in config.json.", name)
		}
		if err := pins.set(name, ""); err != nil {
			return "Could not update the MCP pin file: " + err.Error()
		}
		return fmt.Sprintf("Cleared the pin for %s. Restart ocode: its current tool definitions will be approved and pinned on connect.", name)
	}
	names := make([]string, 0, len(m.cfg.MCPServers))
	for name := range m.cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "No MCP servers configured (mcp_servers in config.json)."
	}
	var b strings.Builder
	b.WriteString("MCP servers:\n")
	for _, name := range names {
		cfg := m.cfg.MCPServers[name]
		state := "connected"
		switch {
		case cfg.Disabled:
			state = "disabled"
		case !cfg.Trusted:
			state = "untrusted (set trusted=true to enable)"
		}
		pin := "not pinned"
		if h, ok := pins.get(name); ok {
			pin = "pinned " + h[:12]
		}
		fmt.Fprintf(&b, "  %s — %s, %s\n", name, state, pin)
	}
	b.WriteString("\nTool definitions are pinned on first connect; a change disables the server until /mcp repin <name>.")
	return b.String()
}
