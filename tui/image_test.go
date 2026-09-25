package tui

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func imageWorkspace(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	tools.SetWorkspaceRoot(root)
	t.Cleanup(func() { tools.SetWorkspaceRoot("") })
	t.Chdir(root)
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	os.WriteFile(filepath.Join(root, "shot.png"), buf.Bytes(), 0o644)
}

func TestAttachMentionsImageNeedsVision(t *testing.T) {
	imageWorkspace(t)
	m := &Model{}

	block, images := m.attachMentions("what is in @shot.png")
	if len(images) != 0 || !strings.Contains(block, "does not accept images") {
		t.Fatalf("non-vision model: images=%d block=%q", len(images), block)
	}

	yes := true
	m.profile.SupportsVision = &yes
	block, images = m.attachMentions("what is in @shot.png")
	if len(images) != 1 || !strings.Contains(block, "attached to the user's message") {
		t.Fatalf("vision model: images=%d block=%q", len(images), block)
	}
}

func TestToolsForModeHidesReadImageWithoutVision(t *testing.T) {
	m := &Model{tools: tools.NewRegistry(), mode: ExploreMode}
	m.tools.Register(tools.ReadImageTool())
	has := func() bool {
		for _, d := range m.toolsForMode() {
			if d.Function.Name == "read_image" {
				return true
			}
		}
		return false
	}
	if has() {
		t.Fatal("read_image offered to a model without vision")
	}
	yes := true
	m.profile.SupportsVision = &yes
	if !has() {
		t.Fatal("read_image hidden from a vision model")
	}
}

func TestPrunedToolResultsDropImages(t *testing.T) {
	m := &Model{history: []api.Message{
		{Role: "assistant"},
		{Role: "tool", Content: "old", Images: []string{"AAAA"}},
		{Role: "tool", Content: "new", Images: []string{"BBBB"}},
	}, prunedThrough: 2}
	got := m.deriveModelMessages()
	if len(got[1].Images) != 0 || len(got[2].Images) != 1 {
		t.Fatalf("images after pruning: old=%d new=%d", len(got[1].Images), len(got[2].Images))
	}
	if len(m.history[1].Images) != 1 {
		t.Fatal("pruning must only change the projection, not the log")
	}
}

func TestImagesCountTowardTokenEstimate(t *testing.T) {
	plain := estimateMsgTokens(api.Message{Role: "user", Content: "hi"})
	withImg := estimateMsgTokens(api.Message{Role: "user", Content: "hi", Images: []string{"x"}})
	if withImg-plain != imageTokenEstimate {
		t.Fatalf("image added %d tokens, want %d", withImg-plain, imageTokenEstimate)
	}
}
