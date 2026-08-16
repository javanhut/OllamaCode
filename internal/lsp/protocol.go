package lsp

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
)

// Position is LSP's zero-based line and UTF-16 character offset. Callers hand
// this package one-based lines and columns (what an editor and the tools layer
// speak) and conversion happens at the boundary in Manager.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// locationLink is the newer definition reply shape. Servers pick one or the
// other, so both are decoded and normalized to Location.
type locationLink struct {
	TargetURI            string `json:"targetUri"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
	TargetRange          Range  `json:"targetRange"`
}

// Severity values as defined by the protocol.
const (
	SeverityError   = 1
	SeverityWarning = 2
)

type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Code     any    `json:"code"`
	Source   string `json:"source"`
	Message  string `json:"message"`
}

type publishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

func (c *Client) storeDiagnostics(raw json.RawMessage) {
	var params publishDiagnosticsParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	// An empty list is meaningful: it is how a server says "this file is clean
	// now", so it replaces rather than merges.
	c.diags[params.URI] = params.Diagnostics
}

func (c *Client) diagnosticsFor(uri string) ([]Diagnostic, bool) {
	c.diagMu.RLock()
	defer c.diagMu.RUnlock()
	d, ok := c.diags[uri]
	return d, ok
}

// pathToURI converts an absolute filesystem path to the file:// URI servers
// expect. Each segment is escaped, so spaces and other awkward characters in a
// path do not produce a URI the server silently ignores.
func pathToURI(path string) string {
	path = filepath.ToSlash(path)
	var b strings.Builder
	b.WriteString("file://")
	for i, segment := range strings.Split(path, "/") {
		if i > 0 {
			b.WriteString("/")
		}
		b.WriteString(url.PathEscape(segment))
	}
	return b.String()
}

// uriToPath is the inverse, tolerant of servers that return an unescaped URI.
func uriToPath(uri string) string {
	trimmed := strings.TrimPrefix(uri, "file://")
	if decoded, err := url.PathUnescape(trimmed); err == nil {
		return decoded
	}
	return trimmed
}

// decodeLocations normalizes the three shapes a definition or references reply
// can take: a single Location, an array of Locations, or an array of
// LocationLinks. A reply that matches none of them yields no locations rather
// than an error — the caller has a fallback and an empty answer is honest.
func decodeLocations(raw json.RawMessage) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var many []Location
	if err := json.Unmarshal(raw, &many); err == nil && len(many) > 0 && many[0].URI != "" {
		return many
	}
	var links []locationLink
	if err := json.Unmarshal(raw, &links); err == nil && len(links) > 0 && links[0].TargetURI != "" {
		out := make([]Location, 0, len(links))
		for _, link := range links {
			rng := link.TargetSelectionRange
			if rng == (Range{}) {
				rng = link.TargetRange
			}
			out = append(out, Location{URI: link.TargetURI, Range: rng})
		}
		return out
	}
	var one Location
	if err := json.Unmarshal(raw, &one); err == nil && one.URI != "" {
		return []Location{one}
	}
	return nil
}

// decodeHover flattens the several shapes hover contents can take (a plain
// string, a marked string with a language, or an array of either) into text.
func decodeHover(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var hover struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &hover); err != nil || len(hover.Contents) == 0 {
		return ""
	}
	return flattenMarkup(hover.Contents)
}

func flattenMarkup(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var markup struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &markup); err == nil && markup.Value != "" {
		return strings.TrimSpace(markup.Value)
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out []string
		for _, part := range parts {
			if s := flattenMarkup(part); s != "" {
				out = append(out, s)
			}
		}
		return strings.TrimSpace(strings.Join(out, "\n"))
	}
	return ""
}
