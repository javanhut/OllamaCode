package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tracepkg "github.com/javanhut/ollama_code/internal/trace"
)

// A rating that cannot be recorded must say so: a toast is the only feedback
// the user gets, and a silently dropped verdict never reaches the dataset.
func TestRateCommandRefusesWhenNothingToRate(t *testing.T) {
	m := &Model{}
	m.rateCommand("good")
	if !strings.Contains(m.toast, "no completed turn") {
		t.Fatalf("toast %q, want the no-completed-turn refusal", m.toast)
	}

	m.ratedTo = 5
	m.rateCommand("good")
	if !strings.Contains(m.toast, "tracing is off") {
		t.Fatalf("toast %q, want the tracing-off refusal", m.toast)
	}

	path := filepath.Join(t.TempDir(), "trace.jsonl")
	rec, err := tracepkg.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m.trace = rec
	m.ratedFrom = 3
	m.rateCommand("bad wrong file")
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if m.turnRating != "bad" {
		t.Fatalf("turnRating %q, want bad", m.turnRating)
	}
	var events []tracepkg.Event
	if err := tracepkg.Replay(path, func(ev tracepkg.Event) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "turn_rating" || events[0].Turn != 0 {
		t.Fatalf("recorded %#v, want one turn_rating carrying no Turn", events)
	}
	meta := events[0].Metadata
	if meta["rating"] != "bad" || meta["note"] != "wrong file" || meta["turn"] != 5.0 || meta["from_turn"] != 3.0 {
		t.Fatalf("metadata %#v, want the rating, note and the 3..5 generation span", meta)
	}
}

// A queued follow-up starts the next turn from inside endTurnTail itself, so
// the turn that just completed has to survive the guard reset that starts —
// otherwise typing /rate right after any turn with something queued behind it
// is refused, and that turn can never be rated at all.
func TestRateableTurnSurvivesTheNextTurnStarting(t *testing.T) {
	m := &Model{ratedFrom: 3, ratedTo: 5, turnRating: "good", turnGen: 5}
	m.resetTurnGuards()
	if m.ratedFrom != 3 || m.ratedTo != 5 || m.turnRating != "good" {
		t.Fatalf("rateable span after the next turn started = %d..%d rated %q, want 3..5 rated good",
			m.ratedFrom, m.ratedTo, m.turnRating)
	}
}

// Bare /rate must not report a turn as "unrated" — which reads as an
// invitation — in a state where the verdict it invites gets refused.
func TestBareRateAgreesWithTheVerdictItInvites(t *testing.T) {
	m := &Model{}
	m.rateCommand("")
	if !strings.Contains(m.toast, "no completed turn") {
		t.Fatalf("bare /rate with nothing rateable said %q, want the same refusal a verdict gets", m.toast)
	}
}
