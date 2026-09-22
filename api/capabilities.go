package api

// ProviderCapabilities describes wire-level features independently from model
// quality. ModelProfile still decides whether a particular model should use
// parallel tools or thinking; this tells the harness what the endpoint can
// represent safely.
type ProviderCapabilities struct {
	NativeToolCalls  bool `json:"native_tool_calls"`
	StructuredOutput bool `json:"structured_output"`
	ParallelTools    bool `json:"parallel_tools"`
	ThinkingStream   bool `json:"thinking_stream"`
	Usage            bool `json:"usage"`
}

type CapabilityProvider interface {
	ProviderCapabilities() ProviderCapabilities
}

func (o OllamaHost) ProviderCapabilities() ProviderCapabilities {
	if o.IsCursor() {
		return ProviderCapabilities{Usage: true}
	}
	if o.IsOpenAI() {
		// These are protocol capabilities. Individual compatible providers may
		// reject an optional feature; callers retain graceful fallbacks.
		return ProviderCapabilities{
			NativeToolCalls: true, StructuredOutput: true, ParallelTools: true,
			ThinkingStream: true, Usage: true,
		}
	}
	return ProviderCapabilities{
		NativeToolCalls: true, StructuredOutput: true, ParallelTools: true,
		ThinkingStream: true, Usage: true,
	}
}
