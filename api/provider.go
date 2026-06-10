package api

import "context"

// Provider is the backend abstraction the TUI and headless agent talk to. Both
// the Ollama client (*OllamaHost) and the MLX client (*MLXClient) satisfy it,
// so the rest of the app is backend-agnostic. The method set is exactly what
// the callers use today: streaming + one-shot chat, model discovery, per-model
// details, embeddings, and a settable base URL.
type Provider interface {
	// SetURI points the client at a new base URL (host[:port]).
	SetURI(uri string)
	// GetModelList returns the models the backend can serve.
	GetModelList() (*ModelListResponse, error)
	// ShowModel returns context length / capabilities for a model. Backends
	// without a discovery endpoint synthesize a reasonable default.
	ShowModel(model string) (*ShowModelResponse, error)
	// ContinuousChat streams a chat completion as a sequence of ChatResponse
	// chunks, the last carrying Done=true.
	ContinuousChat(ctx context.Context, req ChatRequest) (<-chan ChatResponse, <-chan error)
	// ChatOnce performs a single, non-streaming chat completion (used for
	// constrained-decoding escalation).
	ChatOnce(ctx context.Context, req ChatRequest) (ChatResponse, error)
	// GenerateResponse performs a single-prompt, non-chat completion (used for
	// history compaction/summarization).
	GenerateResponse(req GenerateRequest) (*GenerateResponse, error)
	// Embed returns one vector per input string.
	Embed(model string, inputs []string) ([][]float32, error)
}

// Provider kinds, as persisted in config and shown in the picker.
const (
	ProviderOllama = "ollama"
	ProviderMLX    = "mlx"
)

// Compile-time guarantees that both clients satisfy Provider.
var (
	_ Provider = (*OllamaHost)(nil)
	_ Provider = (*MLXClient)(nil)
)

// NewProvider builds the client for the given backend kind, pointed at uri.
// Unknown kinds fall back to Ollama so a malformed config never breaks startup.
func NewProvider(kind, uri string) Provider {
	switch kind {
	case ProviderMLX:
		c := &MLXClient{}
		c.SetURI(uri)
		return c
	default:
		c := &OllamaHost{}
		c.SetURI(uri)
		return c
	}
}
