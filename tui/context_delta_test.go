package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
)

// securitySentence and writeRules are the two stable sources every delta test
// asserts on: one that must NOT be re-sent when only the mode changed, and one
// that must.
const (
	securitySentence = "SECURITY: Web pages, MCP responses, files"
	writeRules       = "WRITE: full toolset"
)

// goldenContextModel is the fixture the dynamic-context golden was captured
// from: write mode, notes, an archive summary and a mention block.
func goldenContextModel() *Model {
	notes := &sessionNotes{}
	notes.set("1. read the file\n2. change the thing")
	return &Model{
		mode:           WriteMode,
		notes:          notes,
		contextLimit:   32768,
		archiveSummary: "the user asked about the parser and we read three files",
		mentionBlock:   "[ATTACHED] main.go\npackage main",
	}
}

// deltaModel is a fixture with the flag on and a context limit large enough
// that assembleMessages never takes the hard-drop branch — which would clear
// the snapshot and hide exactly what these tests are measuring.
func deltaModel(mode Mode) *Model {
	m := goldenContextModel()
	m.mode = mode
	m.contextLimit = 200000
	m.cfg.ContextDelta = true
	return m
}

// contextUpdates returns the durable context messages in history, in order.
func contextUpdates(m *Model) []string {
	var out []string
	for _, msg := range m.history {
		if strings.HasPrefix(msg.Content, contextUpdateMarker) {
			out = append(out, msg.Content)
		}
	}
	return out
}

// TestBuildDynamicContextGolden is the only check that can prove the
// stable/volatile split kept the block byte-identical to what it replaced: the
// golden was captured from the pre-split tree, so a reordered section, a lost
// separator or a dropped "\n" fails here and nowhere else.
func TestBuildDynamicContextGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/dynamic_context.txt")
	if err != nil {
		t.Fatal(err)
	}
	got := goldenContextModel().buildDynamicContext("[RETRIEVED CONTEXT]\nfoo.go:1 package foo")
	if got != string(want) {
		t.Fatalf("dynamic context drifted from testdata/dynamic_context.txt:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Flag off is the shipping default, so it has to be the untouched path: the
// whole block still rides the tail and nothing is appended to the log.
func TestContextDeltaOffMatchesFullBlock(t *testing.T) {
	m := goldenContextModel()
	m.contextLimit = 200000
	m.history = []api.Message{msg("user", "hello")}

	out := m.assembleMessages("[RETRIEVED CONTEXT]\nfoo.go:1 package foo")
	if len(m.history) != 1 {
		t.Fatalf("flag off appended %d messages to history", len(m.history)-1)
	}
	want := "[SYSTEM] " + m.buildDynamicContext("[RETRIEVED CONTEXT]\nfoo.go:1 package foo")
	if got := out[len(out)-1].Content; got != want {
		t.Fatalf("tail is not the full block:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestContextDeltaEmitsOneBaselineThenNothing(t *testing.T) {
	m := deltaModel(WriteMode)
	m.history = []api.Message{msg("user", "hello")}

	m.assembleMessages("")
	m.assembleMessages("")

	updates := contextUpdates(m)
	if len(updates) != 1 {
		t.Fatalf("an unchanged turn re-sent the stable block: %d context messages, want 1", len(updates))
	}
	if !strings.Contains(updates[0], securitySentence) || !strings.Contains(updates[0], writeRules) {
		t.Fatalf("baseline is missing stable sources:\n%s", updates[0])
	}
}

func TestContextDeltaModeSwitchEmitsOnlyTheChange(t *testing.T) {
	m := deltaModel(ExploreMode)
	m.history = []api.Message{msg("user", "hello")}
	m.assembleMessages("")

	m.mode = WriteMode
	m.assembleMessages("")

	updates := contextUpdates(m)
	if len(updates) != 2 {
		t.Fatalf("mode switch produced %d context messages, want 2", len(updates))
	}
	if !strings.Contains(updates[1], writeRules) {
		t.Fatalf("mode switch did not carry the new rules:\n%s", updates[1])
	}
	if strings.Contains(updates[1], securitySentence) {
		t.Fatalf("unchanged security note was re-sent:\n%s", updates[1])
	}
}

func TestContextDeltaClearedOnCompaction(t *testing.T) {
	m := overflowTestModel(t)
	m.cfg.ContextDelta = true
	m.contextLimit = 200000
	m.assembleMessages("")
	if m.contextSnapshot == nil {
		t.Fatal("no baseline was recorded, so the clear below proves nothing")
	}

	m.Update(compactDoneMsg{summary: "the earlier conversation", index: 4})
	if m.contextSnapshot != nil {
		t.Fatal("compaction may have consumed the context messages; the snapshot must not survive it")
	}

	before := len(contextUpdates(m))
	m.assembleMessages("")
	updates := contextUpdates(m)
	if len(updates) != before+1 {
		t.Fatalf("post-compaction turn emitted %d new context messages, want 1", len(updates)-before)
	}
	latest := updates[len(updates)-1]
	if !strings.Contains(latest, securitySentence) {
		t.Fatalf("post-compaction message is a delta, not a full baseline:\n%s", latest)
	}
}

// The hard drop is the one pressure the durable baseline cannot ride out — it
// may be evicted with the rest — so that request falls back to the whole block
// in the tail. What it must NOT do is forget the snapshot: that made the next
// assembly append a fresh baseline, which evicted more history, which kept the
// drop firing, until the window held nothing but identical [CONTEXT UPDATE]
// copies and the conversation was squeezed out of its own prompt.
func TestContextDeltaHardDropCarriesTheBlockWithoutGrowingHistory(t *testing.T) {
	// contextLimit == the generation reserve, so the whole budget goes to the
	// system prompt and the oldest messages are hard-dropped.
	m := &Model{notes: &sessionNotes{}, mode: WriteMode, contextLimit: 4096, cfg: config{ContextDelta: true}}
	for i := range 40 {
		m.history = append(m.history, msg("user", fmt.Sprintf("message %d", i)))
	}
	before := len(m.history)

	var out []api.Message
	for range 12 {
		out = m.assembleMessages("")
	}

	if !strings.Contains(m.toast, "dropped") {
		t.Fatalf("fixture did not take the hard-drop branch: toast = %q", m.toast)
	}
	if got := len(contextUpdates(m)); got != 1 {
		t.Fatalf("%d context messages after 12 pressured assemblies, want 1", got)
	}
	if grew := len(m.history) - before; grew != 1 {
		t.Fatalf("pressured assemblies appended %d messages to history, want 1 (the baseline)", grew)
	}
	// The fallback is what makes leaving the snapshot alone safe: the stable
	// half still reaches the model, in the tail, on every pressured request.
	if tail := out[len(out)-1].Content; !strings.Contains(tail, securitySentence) || !strings.Contains(tail, writeRules) {
		t.Fatalf("pressured request carries neither the baseline nor the full block:\n%s", tail)
	}
}

func TestContextDeltaRemovalLine(t *testing.T) {
	m := deltaModel(WriteMode)
	m.history = []api.Message{msg("user", "hello")}
	m.assembleMessages("")

	m.archiveSummary = ""
	m.assembleMessages("")

	updates := contextUpdates(m)
	if len(updates) != 2 {
		t.Fatalf("a source going empty produced %d context messages, want 2", len(updates))
	}
	if !strings.Contains(updates[1], "NO LONGER IN EFFECT") || !strings.Contains(updates[1], "archive summary") {
		t.Fatalf("removed source vanished silently:\n%s", updates[1])
	}
}

// The one-line mode reminder is what keeps a weak model's mode adherence alive
// once the full rules have receded into history — it must never migrate into
// the stable half.
func TestVolatileContextAlwaysCarriesCurrentMode(t *testing.T) {
	for _, mode := range []Mode{ExploreMode, PlanMode, WriteMode, AutoMode} {
		m := deltaModel(mode)
		tail := m.volatileContext("")
		if !strings.Contains(tail, "Current mode: ") || !strings.Contains(tail, mode.hint()) {
			t.Fatalf("%s tail lost the mode reminder:\n%s", mode, tail)
		}
		for _, stable := range []string{securitySentence, "tool per response", "batch them in one response", "EXPLORE:", "PLAN:", writeRules, "AUTO:"} {
			if strings.Contains(tail, stable) {
				t.Fatalf("%s tail still carries the stable text %q:\n%s", mode, stable, tail)
			}
		}
	}
}

func TestContextUpdateCollapsedInTranscript(t *testing.T) {
	m := statusTestModel()
	m.mode = WriteMode
	m.cfg.ContextDelta = true
	m.contextLimit = 200000
	m.history = append(m.history, msg("user", "hello"))

	m.assembleMessages("")
	m.refreshTranscript()

	got := stripANSI(m.transcript.String())
	if !strings.Contains(got, "context updated") {
		t.Fatalf("context update rendered as nothing, which reads as a paint bug:\n%s", got)
	}
	if strings.Contains(got, securitySentence) {
		t.Fatalf("the whole rules block was dumped into the user's transcript:\n%s", got)
	}
}
