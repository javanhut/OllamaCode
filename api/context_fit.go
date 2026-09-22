package api

import (
	"maps"
	"strings"
	"sync"
)

// minContextFloor is the smallest num_ctx the fit search will try. Below this a
// coding turn cannot hold its own system prompt, so an honest failure beats
// retrying into a window nothing fits in.
const minContextFloor = 4096

// A host sized for the model's full advertised window (131k on a modern local
// model) asks llama.cpp for a KV cache that often does not fit in VRAM — the
// same model answers `ollama run` fine because the CLI sends no num_ctx and
// gets the server default. The only thing that distinguishes "too big" from
// "fits" is trying, so a request that dies allocating memory is retried with
// num_ctx halved until it loads or hits the floor, and the value that worked
// is remembered so the search is paid once per model, not once per turn.
//
// ponytail: process-lifetime memory, no VRAM probing. If the ceiling should
// survive restarts, the TUI persists it into the model profile (tui/profile.go).
var contextCeilings struct {
	sync.RWMutex
	byModel map[string]int
}

func ceilingKey(uri, model string) string { return uri + "\x00" + model }

// ContextCeiling reports the largest num_ctx known to load on this host+model,
// or 0 if no allocation failure has been seen yet.
func ContextCeiling(uri, model string) int {
	contextCeilings.RLock()
	defer contextCeilings.RUnlock()
	return contextCeilings.byModel[ceilingKey(uri, model)]
}

// ContextCeiling reports the learned num_ctx ceiling for model on this host.
func (o OllamaHost) ContextCeiling(model string) int { return ContextCeiling(o.uri, model) }

func rememberCeiling(uri, model string, n int) {
	contextCeilings.Lock()
	defer contextCeilings.Unlock()
	if contextCeilings.byModel == nil {
		contextCeilings.byModel = map[string]int{}
	}
	key := ceilingKey(uri, model)
	if prev, ok := contextCeilings.byModel[key]; !ok || n < prev {
		contextCeilings.byModel[key] = n
	}
}

// ForgetContextCeiling drops what was learned for this host+model, so an
// explicit user override (`/model ctx`) gets a real attempt instead of being
// silently clamped to a ceiling measured when the GPU was busier.
func (o OllamaHost) ForgetContextCeiling(model string) {
	contextCeilings.Lock()
	defer contextCeilings.Unlock()
	delete(contextCeilings.byModel, ceilingKey(o.uri, model))
}

func numCtxOf(req ChatRequest) (int, bool) {
	switch n := req.Options["num_ctx"].(type) {
	case int:
		return n, n > 0
	case int64:
		return int(n), n > 0
	case float64:
		return int(n), n > 0
	}
	return 0, false
}

// setNumCtx clones the options map before writing: it belongs to the caller,
// and a retry must not mutate the request they still hold.
func setNumCtx(req *ChatRequest, n int) {
	opts := maps.Clone(req.Options)
	if opts == nil {
		opts = map[string]any{}
	}
	opts["num_ctx"] = n
	req.Options = opts
}

// fitNumCtx lowers req to the ceiling already learned for this host+model, so
// only the first request of a session pays for the search.
func fitNumCtx(req *ChatRequest, uri string) {
	limit := ContextCeiling(uri, req.Model)
	if limit <= 0 {
		return
	}
	if n, ok := numCtxOf(*req); ok && n > limit {
		setNumCtx(req, limit)
	}
}

// shrinkForMemory halves req's num_ctx after the host failed to allocate, and
// reports the new value. False means the error was something else, or num_ctx
// is already at the floor and shrinking further cannot help.
func shrinkForMemory(req *ChatRequest, uri string, err error) (int, bool) {
	if !IsMemoryFailure(err) {
		return 0, false
	}
	n, ok := numCtxOf(*req)
	if !ok || n <= minContextFloor {
		return 0, false
	}
	next := max(n/2, minContextFloor)
	setNumCtx(req, next)
	rememberCeiling(uri, req.Model, next)
	return next, true
}

// memoryFailureMarkers are the phrases a host puts in the body when the model
// or its KV cache would not fit in memory. The status code cannot classify it —
// Ollama answers a failed llama-server startup with the same 500 it uses for
// every other internal fault.
var memoryFailureMarkers = []string{
	"out of memory",
	"cudamalloc failed",
	"failed to allocate",
	"unable to allocate",
	"insufficient memory",
	"requires more system memory",
}

// IsMemoryFailure reports whether err is the host failing to allocate memory
// for the model or its context. Unlike a transport failure it is not transient:
// the identical request fails identically every time, so the only useful
// response is a smaller num_ctx — which ContinuousChat and ChatOnce already
// tried down to the floor before letting the error out.
func IsMemoryFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range memoryFailureMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
