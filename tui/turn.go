package tui

import (
	"context"
	"time"

	"github.com/javanhut/ollama_code/api"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/tools"
)

// Turn lifetime. Every piece of work a turn starts — the model stream, a tool
// batch, the compile check, a RAG lookup, a compaction pass — belongs to the
// turn, and leaving the turn early cancels all of it, the way a statechart
// cancels the work a state invoked when the state is exited. Before this,
// each teardown site (esc, /rewind, /clear) cancelled only the stream, and a
// compile check or compaction ran on while its result was either ignored or,
// worse, acted on by a turn that no longer existed.

// turnPhase is where the current turn is. A turn moves
//
//	idle → retrieving → streaming ⇄ tools → verifying → idle
//
// with repair rounds going verifying → streaming, and any phase able to drop
// back to idle (done, error, interrupt, a pause for the user). One value
// replaces the streaming/verifying/retrieving flags, which could all be true
// at once and were each checked in a slightly different subset.
//
// Compaction is not a phase. It runs alongside a turn (a proactive pass
// starts with the stream), so it is its own region, m.compacting.
type turnPhase int

const (
	phaseIdle       turnPhase = iota
	phaseRetrieving           // embedding the request for auto-RAG before the model call
	phaseStreaming            // the model is generating
	phaseTools                // a tool batch is running or awaiting approval (m.pending)
	phaseVerifying            // the compile/test check is running
)

func (p turnPhase) String() string {
	switch p {
	case phaseRetrieving:
		return "retrieving"
	case phaseStreaming:
		return "streaming"
	case phaseTools:
		return "tools"
	case phaseVerifying:
		return "verifying"
	}
	return "idle"
}

// setPhase moves the turn to p, recording the transition in the trace so a
// --debug log shows the turn's path, not just its requests.
func (m *Model) setPhase(p turnPhase, reason string) {
	if m.phase == p {
		return
	}
	if m.trace != nil {
		_ = m.trace.Record(tracepkg.Event{Kind: "phase", Turn: m.turnGen, Model: m.modelName,
			Metadata: map[string]any{"from": m.phase.String(), "to": p.String(), "reason": reason}})
	}
	m.phase = p
}

// turnInProgress reports whether a turn is running. A message typed now waits
// in the queue instead of starting a second turn beside it.
func (m *Model) turnInProgress() bool { return m.phase != phaseIdle }

// generating reports whether the model's reply is on screen as it arrives:
// streaming, or its tool calls running.
func (m *Model) generating() bool { return m.phase == phaseStreaming || m.phase == phaseTools }

// turnActive reports whether any work is in flight, turn or background: esc
// and ctrl+c interrupt when it is true, and nothing new starts on its own
// until it is false.
func (m *Model) turnActive() bool { return m.turnInProgress() || m.compacting }

// startToolBatch makes calls the pending batch, sharing one cancellable
// lifetime, and moves the turn to the tools phase.
func (m *Model) startToolBatch(calls []tools.ToolCall) {
	ctx, cancel := context.WithCancel(context.Background())
	m.setPhase(phaseTools, "tool calls")
	m.pending = &pendingBatch{
		ctx:     ctx,
		cancel:  cancel,
		calls:   calls,
		results: make([]api.Message, len(calls)),
		started: make([]bool, len(calls)),
		gen:     m.turnGen,
	}
}

// dropPending ends the current tool batch, stopping any call still running.
func (m *Model) dropPending() {
	if m.pending != nil && m.pending.cancel != nil {
		m.pending.cancel()
	}
	m.pending = nil
}

// abandonTurnWork cancels everything the current turn has in flight and
// clears the flags that tracked it. Results already on their way are
// orphaned twice over: turnGen covers the stream and tools, workEpoch covers
// the compile check, retrieval and compaction.
func (m *Model) abandonTurnWork() {
	if m.stream != nil && m.stream.cancel != nil {
		m.stream.cancel()
	}
	m.dropPending()
	if m.verifyCancel != nil {
		m.verifyCancel()
		m.verifyCancel = nil
	}
	if m.compactCancel != nil {
		m.compactCancel()
		m.compactCancel = nil
	}
	m.turnGen++
	m.workEpoch++
	m.setPhase(phaseIdle, "abandoned")
	m.stream = nil
	m.compacting = false
	// A turn waiting on compaction to recover from an overflow is gone; the
	// pass that would have retried it was just cancelled.
	m.overflowErr = nil
	m.busySince = time.Time{}
}
