package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/tools"
)

type mcpServerConfig struct {
	Command         string   `json:"command"`
	Args            []string `json:"args,omitempty"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	ReadOnly        bool     `json:"read_only,omitempty"`
	SmallModelSafe  bool     `json:"small_model_safe,omitempty"`
	Disabled        bool     `json:"disabled,omitempty"`
}

func connectMCPServers(configs map[string]mcpServerConfig, registry *tools.Registry) ([]*tools.ExternalServer, []string) {
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	var servers []*tools.ExternalServer
	var warnings []string
	for _, name := range names {
		cfg := configs[name]
		if cfg.Disabled {
			continue
		}
		server, err := tools.NewExternalServer(name, cfg.Command, cfg.Args...)
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
				for _, tool := range definitions {
					if _, exists := registry.Lookup(tool.Function.Name); exists {
						err = fmt.Errorf("tool name collision: %s", tool.Function.Name)
						break
					}
					registry.Register(tool)
				}
			}
		}
		cancel()
		if err != nil {
			_ = server.Close()
			warnings = append(warnings, fmt.Sprintf("MCP %s: %v", name, err))
			continue
		}
		servers = append(servers, server)
	}
	return servers, compactWarnings(warnings)
}

func compactWarnings(warnings []string) []string {
	for i, warning := range warnings {
		warnings[i] = strings.TrimSpace(warning)
	}
	return warnings
}
