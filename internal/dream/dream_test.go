package dream

import (
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
)

func TestParse(t *testing.T) {
	out := Parse(`{"summary":" thought about x ","ideas":[{"title":"t","detail":"d"}],"long_term_facts":["f"]}`)
	if out.Summary != "thought about x" || len(out.Ideas) != 1 || out.LongTermFacts[0] != "f" || out.Canceled {
		t.Fatalf("structured reply misparsed: %+v", out)
	}
	if out := Parse("just some prose"); out.Summary != "just some prose" || out.Canceled {
		t.Fatalf("free text must become the summary: %+v", out)
	}
	if !Parse("  ").Canceled {
		t.Fatal("an empty reply is a canceled dream")
	}
}

func TestPromptClipsAndBounds(t *testing.T) {
	var history []api.Message
	for i := range 30 {
		history = append(history, api.Message{Role: "user", Content: strings.Repeat("x", 700) + string(rune('a'+i%26))})
	}
	p := Prompt(Context{History: history, Notes: "n", PriorDreams: []string{"earlier"}})
	if strings.Count(p, "user: ") != 16 {
		t.Fatalf("only the last 16 messages are shown, got %d", strings.Count(p, "user: "))
	}
	if strings.Contains(p, strings.Repeat("x", 601)) {
		t.Fatal("messages must be clipped to 600 chars")
	}
	if !strings.Contains(p, "[your current notes]") || !strings.Contains(p, "- earlier") {
		t.Fatal("notes and prior dreams must be included")
	}
}
