package tools

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
			// Same predicate as the shell scrub. Scrubbing what run_shell can
			// see and then handing the same variable over through a dedicated
			// tool is not a boundary — the model would just ask twice.
			if isSecretEnvName(a.Key) {
				return fmt.Sprintf("%s is redacted: it looks like a credential", a.Key), nil
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
			Description: "List environment variables. Anything that looks like a credential is omitted.",
			Parameters:  Schema{Type: "object", Properties: map[string]Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			// The same list the shell gets. This tool used to return os.Environ()
			// verbatim, which put every credential into the transcript, the
			// model's context on every later request, and the trace file.
			return strings.Join(scrubbedEnvironment(), "\n"), nil
		},
	}
}
