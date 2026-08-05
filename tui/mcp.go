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
	Trusted         bool     `json:"trusted,omitempty"`
	WorkDir         string   `json:"work_dir,omitempty"`
	EnvAllow        []string `json:"env_allow,omitempty"`
	CallTimeoutSec  int      `json:"call_timeout_sec,omitempty"`
	MaxResponseKB   int      `json:"max_response_kb,omitempty"`
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
		if !cfg.Trusted {
			warnings = append(warnings, fmt.Sprintf("MCP %s: disabled until trusted=true is explicitly configured", name))
			continue
		}
		maxBytes := cfg.MaxResponseKB * 1024
		server, err := tools.NewExternalServerWithOptions(tools.ExternalServerOptions{
			Name: name, Command: cfg.Command, Args: cfg.Args, WorkDir: cfg.WorkDir,
			EnvAllow: cfg.EnvAllow, MaxResponseBytes: maxBytes,
			CallTimeout: time.Duration(cfg.CallTimeoutSec) * time.Second,
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
				err = registry.ReplacePrefix(server.Namespace(), definitions)
				server.SetToolsChangedHandler(func() {
					refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer refreshCancel()
					updated, refreshErr := server.ListTools(refreshCtx, policy)
					if refreshErr == nil {
						_ = registry.ReplacePrefix(server.Namespace(), updated)
					}
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
		go func(server *tools.ExternalServer) {
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
