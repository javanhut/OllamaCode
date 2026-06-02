package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/internal/semantic"
)

const defaultEmbedModel = "nomic-embed-text"

// RAG tuning.
const (
	ragTopK         = 8
	ragMaxBlockToks = 6000 // hard cap on injected context tokens
)

type ragLoadedMsg struct {
	idx *semantic.Index
	err error
}
type ragRetrievedMsg struct {
	query string
	block string
}
type ragRefreshedMsg struct{ idx *semantic.Index }

// ragEnabled reports whether auto-retrieval is on (default true unless the user
// explicitly disabled it in config).
func (m *Model) ragEnabled() bool {
	return m.cfg.AutoRAG == nil || *m.cfg.AutoRAG
}

func (m *Model) embedModel() string {
	if m.cfg.EmbedModel != "" {
		return m.cfg.EmbedModel
	}
	return defaultEmbedModel
}

// ensureRagIndexCmd lazily loads or builds the semantic index in the background.
// Returns nil if RAG is disabled or the index is already ready/building.
func (m *Model) ensureRagIndexCmd() tea.Cmd {
	if !m.ragEnabled() || m.ragReady || m.ragBuilding {
		return nil
	}
	m.ragBuilding = true
	host := m.host
	model := m.embedModel()
	cwd, _ := os.Getwd()
	return func() tea.Msg {
		embedder := func(in []string) ([][]float32, error) { return host.Embed(model, in) }
		if idx, err := semantic.LoadIndex(cwd); err == nil && idx.Model == model && len(idx.Chunks) > 0 {
			return ragLoadedMsg{idx: idx}
		}
		idx, err := semantic.BuildIndex(cwd, model, embedder)
		if err == nil {
			_ = semantic.SaveIndex(idx)
		}
		return ragLoadedMsg{idx: idx, err: err}
	}
}

// retrieveRAGCmd embeds the query and searches the (immutable) published index
// in the background, returning a formatted, token-budgeted context block. Any
// failure degrades to an empty block so the turn still proceeds.
func (m *Model) retrieveRAGCmd(query string) tea.Cmd {
	idx := m.ragIndex
	host := m.host
	model := m.embedModel()
	budget := m.contextLimit / 8
	if budget > ragMaxBlockToks {
		budget = ragMaxBlockToks
	}
	return func() tea.Msg {
		results, err := idx.Search(query, func(q string) ([]float32, error) {
			embs, err := host.Embed(model, []string{q})
			if err != nil {
				return nil, err
			}
			if len(embs) == 0 {
				return nil, fmt.Errorf("empty embedding response")
			}
			return embs[0], nil
		}, ragTopK)
		if err != nil {
			return ragRetrievedMsg{query: query, block: ""}
		}
		return ragRetrievedMsg{query: query, block: formatRAGBlock(results, budget)}
	}
}

// formatRAGBlock renders search results into a token-budgeted system block.
func formatRAGBlock(results []semantic.Result, maxTokens int) string {
	if len(results) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[RETRIEVED CONTEXT — auto-selected by relevance to your latest message; verify against the live files before trusting]\n")
	used := estimateTokens(b.String())
	included := 0
	for _, r := range results {
		entry := fmt.Sprintf("\n%s:%d-%d (score %.3f)\n%s\n", r.Path, r.StartLine, r.EndLine, r.Score, r.Text)
		t := estimateTokens(entry)
		if used+t > maxTokens && included > 0 {
			break
		}
		b.WriteString(entry)
		used += t
		included++
	}
	if included == 0 {
		return ""
	}
	return b.String()
}

// noteFileChanged records paths whose chunks need re-embedding. Safe to call
// from a tool goroutine (the registry hook); drained on the next user turn.
func (m *Model) noteFileChanged(paths []string) {
	m.ragMu.Lock()
	defer m.ragMu.Unlock()
	if m.ragChanged == nil {
		m.ragChanged = map[string]bool{}
	}
	for _, p := range paths {
		m.ragChanged[p] = true
	}
}

// drainReindexCmd builds a fresh index (Clone + ReindexFile per changed path) in
// the background and delivers it via ragRefreshedMsg. Returns nil when there's
// nothing to do.
func (m *Model) drainReindexCmd() tea.Cmd {
	if !m.ragReady || m.ragIndex == nil {
		return nil
	}
	m.ragMu.Lock()
	if len(m.ragChanged) == 0 {
		m.ragMu.Unlock()
		return nil
	}
	paths := make([]string, 0, len(m.ragChanged))
	for p := range m.ragChanged {
		paths = append(paths, p)
	}
	m.ragChanged = map[string]bool{}
	m.ragMu.Unlock()

	idx := m.ragIndex
	host := m.host
	model := m.embedModel()
	cwd, _ := os.Getwd()
	return func() tea.Msg {
		embedder := func(in []string) ([][]float32, error) { return host.Embed(model, in) }
		next := idx.Clone()
		for _, p := range paths {
			rel := p
			abs := p
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(cwd, p)
			}
			if r, err := filepath.Rel(cwd, abs); err == nil {
				rel = r
			}
			_ = next.ReindexFile(cwd, rel, embedder)
		}
		_ = semantic.SaveIndex(next)
		return ragRefreshedMsg{idx: next}
	}
}

// ragBlockForTurn returns the retrieval block to inject. It's reused across tool
// re-invokes within a turn and refreshed per user turn by retrieveRAGCmd.
func (m *Model) ragBlockForTurn() string { return m.lastRagBlock }

// applyRagLoaded / applyRagRetrieved / applyRagRefreshed are invoked from Update.
func (m *Model) applyRagLoaded(msg ragLoadedMsg) {
	m.ragBuilding = false
	if msg.err != nil || msg.idx == nil {
		return // RAG silently unavailable (e.g. embed model not pulled)
	}
	m.ragIndex = msg.idx
	m.ragReady = true
}

func (m *Model) applyRagRefreshed(msg ragRefreshedMsg) {
	if msg.idx != nil {
		m.ragIndex = msg.idx
	}
}

// startStreamWithRAGGate decides whether to retrieve first (gating the model
// call by one embed round-trip) or stream immediately. It also kicks index
// build and incremental reindex as needed. Used for true user turns.
func (m *Model) startStreamWithRAGGate(query string) []tea.Cmd {
	var cmds []tea.Cmd
	if c := m.ensureRagIndexCmd(); c != nil {
		cmds = append(cmds, c)
	}
	if c := m.drainReindexCmd(); c != nil {
		cmds = append(cmds, c)
	}
	if m.ragEnabled() && m.ragReady {
		cmds = append(cmds, m.retrieveRAGCmd(query)) // startStream fires on ragRetrievedMsg
	} else {
		cmds = append(cmds, m.startStream())
	}
	return cmds
}
