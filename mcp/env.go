package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func GetEnvTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "env_get",
			Description: "Get the value of an environment variable.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"key": {Type: "string", Description: "The name of the environment variable."},
				},
				Required: []string{"key"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			val := os.Getenv(a.Key)
			if val == "" {
				return fmt.Sprintf("%s is not set", a.Key), nil
			}
			return val, nil
		},
	}
}

func SetEnvTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "env_set",
			Description: "Set an environment variable for the current process.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"key":   {Type: "string", Description: "The name of the environment variable."},
					"value": {Type: "string", Description: "The value to set."},
				},
				Required: []string{"key", "value"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if err := os.Setenv(a.Key, a.Value); err != nil {
				return "", err
			}
			return fmt.Sprintf("set %s=%s", a.Key, a.Value), nil
		},
	}
}

func ListEnvTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "env_list",
			Description: "List all environment variables. Warning: May contain sensitive info.",
			Parameters:  Schema{Type: "object", Properties: map[string]Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return strings.Join(os.Environ(), "\n"), nil
		},
	}
}
