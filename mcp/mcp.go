package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/javanhut/ollama_code/internal/gitignore"
	"github.com/javanhut/ollama_code/internal/semantic"
	"golang.org/x/net/html"
)

// Tool is an Ollama/OpenAI-compatible function definition paired with a local
// handler. The Type/Function fields are what the model sees; Handler runs the
// call locally when the model emits a matching tool_call.
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
	Handler  Handler  `json:"-"`
}

type Function struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  Schema `json:"parameters"`
}

type Schema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required,omitempty"`
}

type Property struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// Handler executes a tool call. args is the raw JSON object the model sent;
// the returned string is fed back to the model as the tool's reply.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// ----- shared web helpers -----

func newWebClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
	}
}

var webUA = "Mozilla/5.0 (compatible; OllamaCode/1.0; +https://github.com/javanhut/ollama_code)"

func webReq(ctx context.Context, method, rawURL string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webUA)
	req.Header.Set("Accept", "text/html,text/plain,*/*")
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	return newWebClient(30 * time.Second).Do(req)
}

// htmlToText extracts visible text content from HTML, stripping all tags and
// scripts/styles. Returns clean, readable text suitable for model consumption.
func htmlToText(r io.Reader, maxBytes int) string {
	doc, err := html.Parse(r)
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	var walk func(*html.Node)
	skip := false
	walk = func(n *html.Node) {
		if buf.Len() >= maxBytes {
			return
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "head", "meta", "link":
				skip = true
				defer func() { skip = false }()
				return
			case "br", "p", "li", "div", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
				if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' {
					buf.WriteByte('\n')
				}
			}
		}
		if n.Type == html.TextNode && !skip {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' && buf.Bytes()[buf.Len()-1] != ' ' {
					buf.WriteByte(' ')
				}
				buf.WriteString(text)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	result := buf.String()
	if len(result) > maxBytes {
		result = result[:maxBytes]
	}
	return strings.TrimSpace(result)
}

// parseDDG extracts search result titles, snippets, and URLs from DuckDuckGo
// HTML response using proper HTML parsing.
func parseDDG(r io.Reader, maxResults int) []string {
	doc, err := html.Parse(r)
	if err != nil {
		return nil
	}
	type ddgResult struct {
		title, snippet, link string
	}
	var results []ddgResult
	var current *ddgResult
	var inSnippet, inTitle bool
	var linkHref string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(results) >= maxResults {
			return
		}
		if n.Type == html.ElementNode {
			switch {
			case n.Data == "a" && hasClass(n, "result__a"):
				// Title link
				for _, attr := range n.Attr {
					if attr.Key == "href" {
						linkHref = attr.Val
						break
					}
				}
				current = &ddgResult{link: linkHref}
				inTitle = true
				// Extract title text from children
				current.title = extractAllText(n)
				inTitle = false

			case n.Data == "a" && hasClass(n, "result__url"):
				if current != nil && current.link == "" {
					// DDG wraps URLs in a result__url <a>
					for _, attr := range n.Attr {
						if attr.Key == "href" {
							current.link = attr.Val
							break
						}
					}
				}

			case n.Data == "a" && hasClass(n, "result__snippet"):
				// Snippet
				if current == nil {
					current = &ddgResult{}
				}
				inSnippet = true
				current.snippet = extractAllText(n)
				inSnippet = false
				if current.title != "" || current.link != "" {
					results = append(results, *current)
				}
				current = nil
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		_ = inSnippet
		_ = inTitle
	}
	walk(doc)

	out := make([]string, 0, len(results))
	for i, r := range results {
		// Clean up and decode the URL
		link := cleanDDGURL(r.link)
		title := strings.TrimSpace(r.title)
		snippet := strings.TrimSpace(r.snippet)
		if title == "" && snippet == "" {
			continue
		}
		line := fmt.Sprintf("%d. %s\n   %s\n   %s", i+1, title, snippet, link)
		out = append(out, line)
	}
	return out
}

func hasClass(n *html.Node, class string) bool {
	for _, attr := range n.Attr {
		if attr.Key == "class" {
			classes := strings.Fields(attr.Val)
			for _, c := range classes {
				if c == class {
					return true
				}
			}
		}
	}
	return false
}

func extractAllText(n *html.Node) string {
	var buf bytes.Buffer
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			buf.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(buf.String())
}

func cleanDDGURL(raw string) string {
	// DDG uses redirect URLs like //duckduckgo.com/l/?uddg=...
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Host == "duckduckgo.com" || strings.HasSuffix(u.Host, ".duckduckgo.com") {
		if uddg := u.Query().Get("uddg"); uddg != "" {
			if decoded, err := url.QueryUnescape(uddg); err == nil {
				return decoded
			}
			return uddg
		}
	}
	return raw
}

// isPlainText checks if a Content-Type header indicates non-HTML text.
func isPlainText(ct string) bool {
	media, _, _ := mime.ParseMediaType(ct)
	switch media {
	case "text/plain", "text/markdown", "text/xml", "application/json", "application/xml":
		return true
	default:
		return strings.HasPrefix(media, "text/") && media != "text/html"
	}
}

// collapseWhitespace normalises whitespace for compact output.
func collapseWhitespace(s string) string {
	var buf bytes.Buffer
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				buf.WriteByte(' ')
				prevSpace = true
			}
		} else {
			buf.WriteRune(r)
			prevSpace = false
		}
	}
	return buf.String()
}

// stripANSI removes ANSI escape sequences from a string (color codes etc.).
func stripANSI(s string) string {
	var buf bytes.Buffer
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		buf.WriteByte(s[i])
		i++
	}
	return buf.String()
}

// extractLinks parses HTML and returns absolute hrefs that share the given
// host. Used by web_crawl for same-domain link discovery.
func extractLinks(r io.Reader, host string) []string {
	doc, err := html.Parse(r)
	if err != nil {
		return nil
	}
	var links []string
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					href := attr.Val
					u, err := url.Parse(href)
					if err != nil {
						continue
					}
					if u.Host == "" {
						u.Host = host
						u.Scheme = "https"
					}
					if u.Host != host {
						continue
					}
					u.Fragment = ""
					key := u.String()
					if !seen[key] {
						seen[key] = true
						links = append(links, key)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return links
}

// Embedder abstracts the Ollama embedding endpoint so semantic tools can
// compute vectors without creating a circular import on api.OllamaHost.
type Embedder interface {
	Embed(model string, inputs []string) ([][]float32, error)
}

// ToolCall mirrors the shape Ollama emits inside a chat response message.
type ToolCall struct {
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Registry holds the tools available for a session.
type Registry struct {
	tools         map[string]Tool
	onFileChanged func([]string)
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// SetFileChangeHook registers a callback invoked with the affected path(s) after
// a file-mutating tool succeeds. Used to keep the semantic index fresh. The
// callback may run on a tool goroutine, so it must be concurrency-safe.
func (r *Registry) SetFileChangeHook(fn func([]string)) { r.onFileChanged = fn }

// fileMutators are the tools whose success should invalidate the semantic index.
var fileMutators = map[string]bool{
	"write_file": true, "edit_file": true, "append_file": true,
	"delete_file": true, "move_file": true, "copy_file": true, "touch": true,
}

// MutatedPaths extracts the path-like arguments of a file-mutating tool call,
// or nil if the tool doesn't mutate files. Exported for checkpoint snapshots.
func MutatedPaths(name string, raw json.RawMessage) []string {
	return mutatedPaths(name, raw)
}

// mutatedPaths extracts the path-like arguments of a file-mutating tool call.
func mutatedPaths(name string, raw json.RawMessage) []string {
	if !fileMutators[name] {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"path", "dest", "destination", "new_path", "to", "source", "src"} {
		if v, ok := m[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (r *Registry) Register(t Tool) {
	if t.Type == "" {
		t.Type = "function"
	}
	r.tools[t.Function.Name] = t
}

// Definitions returns the tool list to send in a ChatRequest, sorted by name
// for stable output.
func (r *Registry) Definitions() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Function.Name < out[j].Function.Name })
	return out
}

// Invoke dispatches a tool call. Returns the handler's reply string or an
// error message suitable for sending back as the tool's response.
func (r *Registry) Invoke(ctx context.Context, call ToolCall) (string, error) {
	t, ok := r.tools[call.Function.Name]
	if !ok {
		if cand, d := r.Nearest(call.Function.Name); cand != "" && (d <= 3 || d <= len(call.Function.Name)/3) {
			return "", fmt.Errorf("unknown tool %q. Did you mean %q? Available tools: %s",
				call.Function.Name, cand, strings.Join(r.Names(), ", "))
		}
		return "", fmt.Errorf("unknown tool %q. Available tools: %s",
			call.Function.Name, strings.Join(r.Names(), ", "))
	}
	if t.Handler == nil {
		return "", fmt.Errorf("tool %q has no handler", call.Function.Name)
	}
	if err := ValidateArgs(t.Function, call.Function.Arguments); err != nil {
		return "", err
	}
	out, err := t.Handler(ctx, call.Function.Arguments)
	if err == nil && r.onFileChanged != nil {
		if paths := mutatedPaths(call.Function.Name, call.Function.Arguments); len(paths) > 0 {
			r.onFileChanged(paths)
		}
	}
	return out, err
}

// DefaultRegistry returns a registry pre-populated with the built-in
// filesystem and shell tools. Add more with Register.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(ReadFileTool())
	r.Register(WriteFileTool())
	r.Register(AppendFileTool())
	r.Register(EditFileTool())
	r.Register(DeleteFileTool())
	r.Register(MoveFileTool())
	r.Register(CopyFileTool())
	r.Register(ListDirectoryTool())
	r.Register(FindFilesTool())
	r.Register(MakeDirectoryTool())
	r.Register(TouchFileTool())
	r.Register(FileInfoTool())
	r.Register(GetWorkingDirectoryTool())
	r.Register(GrepTool())
	r.Register(RunShellTool())
	r.Register(WebFetchTool())
	r.Register(WebSearchTool())
	r.Register(GetProjectTreeTool())
	r.Register(FindSymbolTool())
	r.Register(AskUserTool())
	r.Register(GitStatusTool())
	r.Register(GitDiffTool())
	r.Register(GitLogTool())
	r.Register(GitAddTool())
	r.Register(GitCommitTool())
	r.Register(GitBranchTool())
	r.Register(GitCheckoutTool())
	r.Register(GitPullTool())
	r.Register(GitPushTool())
	r.Register(GitStashTool())
	r.Register(GitMergeTool())
	r.Register(GitResetTool())
	r.Register(GitRemoteTool())
	r.Register(GetEnvTool())
	r.Register(SetEnvTool())
	r.Register(ListEnvTool())
	r.Register(CodeDefinitionTool())
	r.Register(CodeReferencesTool())
	r.Register(CodeHoverTool())
	r.Register(WebCrawlTool())
	r.Register(WebSearchAPITool())
	r.Register(HashFileTool())
	r.Register(ProcessListTool())
	r.Register(ProcessKillTool())
	r.Register(DiskUsageTool())
	return r
}

// ----- built-in tools -----

// calculateHash computes the SHA-256 hash of a file's content.
func calculateHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func HashFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "hash_file",
			Description: "Calculate the SHA-256 hash of a file. Used for Differential State Tracking to detect drift before modification.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the file."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			hash, err := calculateHash(a.Path)
			if err != nil {
				return "", err
			}
			return hash, nil
		},
	}
}

func WebCrawlTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "web_crawl",
			Description: "Fetch a web page and follow links to the same domain up to max_depth. Returns a combined summary of all crawled pages as plain text. Useful for exploring documentation sites.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"url":       {Type: "string", Description: "Starting URL."},
					"max_depth": {Type: "number", Description: "Maximum link depth to follow. Default 1 (just the page itself). Max 3."},
					"max_pages": {Type: "number", Description: "Maximum total pages to fetch. Default 5, max 10."},
				},
				Required: []string{"url"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				URL      string `json:"url"`
				MaxDepth int    `json:"max_depth"`
				MaxPages int    `json:"max_pages"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.URL == "" {
				return "", fmt.Errorf("url is required")
			}
			if a.MaxDepth <= 0 {
				a.MaxDepth = 1
			}
			if a.MaxDepth > 3 {
				a.MaxDepth = 3
			}
			if a.MaxPages <= 0 {
				a.MaxPages = 5
			}
			if a.MaxPages > 10 {
				a.MaxPages = 10
			}

			baseURL, err := url.Parse(a.URL)
			if err != nil {
				return "", fmt.Errorf("invalid starting URL: %w", err)
			}

			var out strings.Builder
			visited := map[string]bool{}
			pages := 0

			var crawl func(pageURL string, depth int)
			crawl = func(pageURL string, depth int) {
				if depth > a.MaxDepth || pages >= a.MaxPages {
					return
				}
				u, err := url.Parse(pageURL)
				if err != nil {
					return
				}
				u.Fragment = ""
				key := u.String()
				if visited[key] {
					return
				}
				visited[key] = true

				resp, err := webReq(ctx, "GET", key, nil)
				if err != nil {
					return
				}
				defer resp.Body.Close()

				body, _ := io.ReadAll(io.LimitReader(resp.Body, 200000))
				text := htmlToText(bytes.NewReader(body), 10000)
				if text == "" {
					return
				}

				pages++
				fmt.Fprintf(&out, "===== [%d/%d] %s (depth %d) =====\n%s\n\n",
					pages, a.MaxPages, key, depth, collapseWhitespace(text))

				// Extract same-domain links for next crawl level
				if depth < a.MaxDepth && pages < a.MaxPages {
					links := extractLinks(bytes.NewReader(body), baseURL.Host)
					for _, link := range links {
						crawl(link, depth+1)
					}
				}
			}

			crawl(a.URL, 1)

			if pages == 0 {
				return "no pages crawled", nil
			}
			return fmt.Sprintf("Crawled %d pages starting from %s\n\n%s", pages, a.URL, out.String()), nil
		},
	}
}

func WebSearchAPITool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "web_search_api",
			Description: "Search the web using Serper.dev API (Google-quality results). Set SEARCH_API_KEY env var with your Serper API key. Falls back to DuckDuckGo scraping if no key is configured. Returns structured results with titles, snippets, and URLs.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"query":       {Type: "string", Description: "The search query."},
					"max_results": {Type: "number", Description: "Maximum number of results. Default 10."},
				},
				Required: []string{"query"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Query      string `json:"query"`
				MaxResults int    `json:"max_results"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Query == "" {
				return "", fmt.Errorf("query is required")
			}
			if a.MaxResults <= 0 {
				a.MaxResults = 10
			}

			apiKey := os.Getenv("SEARCH_API_KEY")
			if apiKey == "" {
				// Fall back to DDG scraping
				ddg := WebSearchTool()
				ddgArgs, _ := json.Marshal(map[string]any{
					"query":       a.Query,
					"max_results": a.MaxResults,
				})
				return ddg.Handler(ctx, ddgArgs)
			}

			reqBody, _ := json.Marshal(map[string]any{
				"q":   a.Query,
				"num": a.MaxResults,
			})
			resp, err := webReq(ctx, "POST", "https://google.serper.dev/search", bytes.NewReader(reqBody))
			if err != nil {
				return "", fmt.Errorf("serper API call failed: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
				return "", fmt.Errorf("serper returned status %d: %s", resp.StatusCode, string(body))
			}

			var serperResp struct {
				Organic []struct {
					Title   string `json:"title"`
					Link    string `json:"link"`
					Snippet string `json:"snippet"`
				} `json:"organic"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&serperResp); err != nil {
				return "", fmt.Errorf("failed to parse serper response: %w", err)
			}

			if len(serperResp.Organic) == 0 {
				return "no results found for query: " + a.Query, nil
			}

			var out []string
			for i, r := range serperResp.Organic {
				if len(out) >= a.MaxResults {
					break
				}
				out = append(out, fmt.Sprintf("%d. %s\n   %s\n   %s", i+1, r.Title, r.Snippet, r.Link))
			}
			return strings.Join(out, "\n"), nil
		},
	}
}

func CodeDefinitionTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "code_definition",
			Description: "Find where a symbol is defined. Give it a file path and line number, and it extracts the symbol name at that position then searches the project for its definition. Works for Go, Rust, Python, C/C++, JS/TS, Zig, and more.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the file containing the symbol."},
					"line": {Type: "number", Description: "Line number where the symbol is used (1-indexed)."},
				},
				Required: []string{"path", "line"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
				Line int    `json:"line"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" || a.Line < 1 {
				return "", fmt.Errorf("path and line (>=1) are required")
			}

			// Read the file and extract the symbol name at the given line
			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			lines := strings.Split(string(data), "\n")
			if a.Line > len(lines) {
				return "", fmt.Errorf("line %d is past end of file (%d lines)", a.Line, len(lines))
			}
			targetLine := lines[a.Line-1]

			// Extract likely symbol: a word that starts with letter/underscore
			re := regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)
			symbols := re.FindAllString(targetLine, -1)
			if len(symbols) == 0 {
				return "", fmt.Errorf("no symbol found on line %d", a.Line)
			}

			// Use the last non-keyword symbol (most likely the interesting one)
			keywords := map[string]bool{"func": true, "fn": true, "def": true, "class": true,
				"struct": true, "type": true, "var": true, "const": true, "let": true,
				"return": true, "if": true, "for": true, "import": true, "pub": true}
			sym := symbols[len(symbols)-1]
			for i := len(symbols) - 1; i >= 0; i-- {
				if !keywords[symbols[i]] {
					sym = symbols[i]
					break
				}
			}

			// Search for definition of this symbol
			pat := fmt.Sprintf(
				`(^|[[:space:]])(func|fn|class|struct|trait|enum|impl|interface|type|var|const|def)[[:space:]]+.*%s([[:space:]]|$)`,
				regexp.QuoteMeta(sym),
			)
			cmd := exec.CommandContext(ctx, "grep", "-rnE", "--color=never",
				"--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=build",
				"--exclude-dir=vendor", "--exclude-dir=target", pat, ".")
			out, _ := cmd.CombinedOutput()
			text := filterCodeMatches(strings.TrimSpace(stripANSI(string(out))), 50)
			if text == "" {
				return fmt.Sprintf("definition of %q not found in project (found on line %d of %s)", sym, a.Line, a.Path), nil
			}
			return fmt.Sprintf("definition(s) for %q (referenced at %s:%d):\n%s",
				sym, a.Path, a.Line, text), nil
		},
	}
}

func CodeReferencesTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "code_references",
			Description: "Find all usages of a symbol across the project. Give it a file and line number; it extracts the symbol name and greps for all references (excluding comments). Use this to understand how a function or type is used before modifying it.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the file containing the symbol."},
					"line": {Type: "number", Description: "Line number where the symbol appears (1-indexed)."},
				},
				Required: []string{"path", "line"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
				Line int    `json:"line"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" || a.Line < 1 {
				return "", fmt.Errorf("path and line (>=1) are required")
			}

			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			lines := strings.Split(string(data), "\n")
			if a.Line > len(lines) {
				return "", fmt.Errorf("line %d is past end of file (%d lines)", a.Line, len(lines))
			}
			targetLine := lines[a.Line-1]

			re := regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*`)
			symbols := re.FindAllString(targetLine, -1)
			if len(symbols) == 0 {
				return "", fmt.Errorf("no symbol found on line %d", a.Line)
			}

			// Filter out keywords
			keywords := map[string]bool{"func": true, "fn": true, "def": true, "class": true,
				"struct": true, "type": true, "var": true, "const": true, "let": true,
				"return": true, "if": true, "for": true, "import": true, "pub": true,
				"package": true, "else": true, "match": true, "switch": true, "case": true}
			sym := symbols[len(symbols)-1]
			for i := len(symbols) - 1; i >= 0; i-- {
				if !keywords[symbols[i]] {
					sym = symbols[i]
					break
				}
			}

			// Grep for all occurrences of this word (word-boundary match via -w)
			cmd := exec.CommandContext(ctx, "grep", "-rnwE", "--color=never",
				"--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=build",
				"--exclude-dir=vendor", "--exclude-dir=target", sym, ".")
			out, _ := cmd.CombinedOutput()
			text := filterCodeMatches(strings.TrimSpace(stripANSI(string(out))), 50)
			if text == "" {
				return fmt.Sprintf("no references to %q found in project", sym), nil
			}
			return fmt.Sprintf("references to %q (comments excluded):\n%s", sym, text), nil
		},
	}
}

func CodeHoverTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "code_hover",
			Description: "Show the definition context surrounding a symbol. Give it a file and line number; it finds the symbol at that line and returns its full definition block — function signature, struct fields, class definition, etc. Use this to quickly understand what something is without reading the whole file.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the file containing the symbol."},
					"line": {Type: "number", Description: "Line number where the symbol appears (1-indexed)."},
				},
				Required: []string{"path", "line"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
				Line int    `json:"line"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" || a.Line < 1 {
				return "", fmt.Errorf("path and line (>=1) are required")
			}

			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			lines := strings.Split(string(data), "\n")
			if a.Line > len(lines) {
				return "", fmt.Errorf("line %d is past end of file (%d lines)", a.Line, len(lines))
			}

			// Find the enclosing definition block
			// Look backwards for the start (func, fn, def, class, struct, type, etc.)
			start := a.Line - 1
			defRe := regexp.MustCompile(`^\s*(func|fn|def|class|struct|enum|trait|impl|interface|type|pub\s+(fn|struct|enum|trait|type))\s`)
			for start > 0 && !defRe.MatchString(lines[start]) {
				start--
			}

			// Forward to find the end (closing brace at same indent, or empty line for Python)
			end := a.Line - 1
			baseIndent := len(lines[start]) - len(strings.TrimLeft(lines[start], " \t"))
			braceCount := 0
			inBlock := false
			hasBraces := strings.Contains(lines[start], "{")
			isPython := strings.HasPrefix(strings.TrimSpace(lines[start]), "def ") || strings.HasPrefix(strings.TrimSpace(lines[start]), "class ")

			if isPython {
				for end < len(lines)-1 {
					end++
					if strings.TrimSpace(lines[end]) == "" {
						continue
					}
					indent := len(lines[end]) - len(strings.TrimLeft(lines[end], " \t"))
					if indent <= baseIndent && end > a.Line-1 {
						end--
						break
					}
				}
			} else if hasBraces {
				for end < len(lines) {
					line := lines[end]
					for _, c := range line {
						if c == '{' {
							braceCount++
							inBlock = true
						} else if c == '}' {
							braceCount--
						}
					}
					if inBlock && braceCount == 0 {
						break
					}
					end++
				}
				if end >= len(lines) {
					end = len(lines) - 1
				}
			} else {
				// Simple: just grab 5 lines of context
				end = start + 5
				if end >= len(lines) {
					end = len(lines) - 1
				}
			}

			if start > end {
				end = start
			}

			var buf strings.Builder
			for i := start; i <= end; i++ {
				fmt.Fprintf(&buf, "%d\t%s\n", i+1, lines[i])
			}
			return fmt.Sprintf("definition context for %s, lines %d-%d:\n%s",
				a.Path, start+1, end+1, strings.TrimRight(buf.String(), "\n")), nil
		},
	}
}

func GetEnvTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "env_get",
			Description: "Get the value of an environment variable.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"key": {Type: "string", Description: "The name of the environment variable."},
				},
				Required: []string{"key"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			val := os.Getenv(a.Key)
			if val == "" {
				return fmt.Sprintf("%s is not set", a.Key), nil
			}
			return val, nil
		},
	}
}

func SetEnvTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "env_set",
			Description: "Set an environment variable for the current process.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"key":   {Type: "string", Description: "The name of the environment variable."},
					"value": {Type: "string", Description: "The value to set."},
				},
				Required: []string{"key", "value"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if err := os.Setenv(a.Key, a.Value); err != nil {
				return "", err
			}
			return fmt.Sprintf("set %s=%s", a.Key, a.Value), nil
		},
	}
}

func ListEnvTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "env_list",
			Description: "List all environment variables. Warning: May contain sensitive info.",
			Parameters:  Schema{Type: "object", Properties: map[string]Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return strings.Join(os.Environ(), "\n"), nil
		},
	}
}

// Defaults for recursive read_file on directories.
const (
	readDirMaxTotalBytes = 2 * 1024 * 1024 // 2 MiB total cap
	readDirMaxFileBytes  = 256 * 1024      // 256 KiB per file
)

// Directories skipped during recursive read_file. These are noisy build/VCS
// artifacts that almost never carry useful source context.
var readDirSkipDirs = map[string]bool{
	".git": true, ".svn": true, ".hg": true, ".bzr": true,
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, "out": true, "bin": true, "obj": true,
	"__pycache__": true, ".venv": true, "venv": true,
	".idea": true, ".vscode": true,
	".next": true, ".nuxt": true,
	"coverage": true, ".cache": true, ".terraform": true,
}

func ReadFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "read_file",
			Description: "Read a file from disk. If the path is a directory, read every text file under it recursively (skipping VCS, build, and vendor dirs) and concatenate the results with per-file headers. Use this tool — not list_directory — whenever you actually want file contents. Optional start_line/end_line apply only to single-file reads.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":       {Type: "string", Description: "Absolute or relative path. May be a file or a directory; directories are read recursively."},
					"start_line": {Type: "number", Description: "First line to read (1-indexed). Single-file reads only."},
					"end_line":   {Type: "number", Description: "Last line to read (1-indexed, inclusive). Single-file reads only."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
				MaxBytes  int    `json:"max_bytes"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			info, err := os.Stat(a.Path)
			if err != nil {
				return "", err
			}
			if info.IsDir() {
				if a.StartLine != 0 || a.EndLine != 0 {
					return "", fmt.Errorf("start_line/end_line are not supported when path is a directory")
				}
				return readDirRecursive(a.Path)
			}
			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			if a.StartLine == 0 && a.EndLine == 0 {
				content := string(data)
				maxBytes := a.MaxBytes
				if maxBytes <= 0 {
					maxBytes = 32768
				}
				if len(content) > maxBytes {
					truncated := content[:maxBytes]
					return fmt.Sprintf("%s\n\n... [truncated at %d bytes; %d total bytes. Use start_line/end_line or set max_bytes higher to read more.]", truncated, maxBytes, len(content)), nil
				}
				return content, nil
			}
			lines := strings.Split(string(data), "\n")
			start := a.StartLine
			end := a.EndLine
			if start < 1 {
				start = 1
			}
			if end < 1 || end > len(lines) {
				end = len(lines)
			}
			if start > end {
				return "", fmt.Errorf("start_line %d > end_line %d", start, end)
			}
			var b strings.Builder
			for i := start; i <= end; i++ {
				fmt.Fprintf(&b, "%d\t%s\n", i, lines[i-1])
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

func readDirRecursive(root string) (string, error) {
	var out strings.Builder
	var (
		filesRead    int
		filesSkipped int
		bytesRead    int
		truncated    bool
	)

	gi := gitignore.NewMatcher(root)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != root && gi.IsIgnored(path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			filesSkipped++
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			filesSkipped++
			return nil
		}
		if bytesRead >= readDirMaxTotalBytes {
			truncated = true
			return filepath.SkipAll
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		f, openErr := os.Open(path)
		if openErr != nil {
			filesSkipped++
			fmt.Fprintf(&out, "===== %s =====\n[error opening: %v]\n\n", rel, openErr)
			return nil
		}

		remaining := readDirMaxTotalBytes - bytesRead
		limit := readDirMaxFileBytes
		if remaining < limit {
			limit = remaining
		}
		buf := make([]byte, limit+1)
		n, readErr := io.ReadFull(f, buf)
		f.Close()
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			filesSkipped++
			fmt.Fprintf(&out, "===== %s =====\n[error reading: %v]\n\n", rel, readErr)
			return nil
		}
		chunk := buf[:n]

		if isBinaryContent(chunk) {
			filesSkipped++
			fmt.Fprintf(&out, "===== %s =====\n[binary file, skipped]\n\n", rel)
			return nil
		}

		fileTruncated := n > limit
		if fileTruncated {
			chunk = chunk[:limit]
		}

		fmt.Fprintf(&out, "===== %s =====\n", rel)
		out.Write(chunk)
		if len(chunk) == 0 || chunk[len(chunk)-1] != '\n' {
			out.WriteByte('\n')
		}
		if fileTruncated {
			fmt.Fprintf(&out, "[truncated after %d bytes]\n", limit)
		}
		out.WriteByte('\n')

		filesRead++
		bytesRead += len(chunk)
		if bytesRead >= readDirMaxTotalBytes {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return "", err
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Recursive read of %s — %d files read, %d skipped, %d bytes",
		root, filesRead, filesSkipped, bytesRead)
	if truncated {
		summary.WriteString(" (stopped at total byte cap)")
	}
	summary.WriteString("\n\n")
	return summary.String() + out.String(), nil
}

func isBinaryContent(b []byte) bool {
	n := len(b)
	if n > 512 {
		n = 512
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

func WriteFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "write_file",
			Description: "Write text to a file, creating it (and parent directories) if needed. Overwrites existing contents.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":    {Type: "string", Description: "Absolute or relative path to the file."},
					"content": {Type: "string", Description: "Full file contents to write."},
				},
				Required: []string{"path", "content"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
				return "", err
			}
			mode := os.FileMode(0o644)
			if info, err := os.Stat(a.Path); err == nil {
				mode = info.Mode().Perm()
			}
			if err := os.WriteFile(a.Path, []byte(a.Content), mode); err != nil {
				return "", err
			}
			hash, _ := calculateHash(a.Path)
			return fmt.Sprintf("wrote %d bytes to %s\nNew Hash: %s", len(a.Content), a.Path, hash), nil
		},
	}
}

func ListDirectoryTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "list_directory",
			Description: "List the immediate entries of a directory (non-recursive). One entry per line; directories are suffixed with '/'. Use ONLY when the user explicitly asks to list/inspect directory structure. If you actually want file contents, call read_file on the directory — it recurses automatically.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Absolute or relative path to the directory. Defaults to the current working directory."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("invalid arguments: %w", err)
				}
			}
			path := a.Path
			if path == "" {
				path = "."
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return "", err
			}
			out := make([]string, 0, len(entries))
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				out = append(out, name)
			}
			sort.Strings(out)
			return joinLines(out), nil
		},
	}
}

func MakeDirectoryTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "make_directory",
			Description: "Create a directory and any missing parent directories (mkdir -p).",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the directory to create."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := os.MkdirAll(a.Path, 0o755); err != nil {
				return "", err
			}
			return "created " + a.Path, nil
		},
	}
}

func TouchFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "touch",
			Description: "Create an empty file if it does not exist, otherwise update its modification time.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the file."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
				return "", err
			}
			now := time.Now()
			if _, err := os.Stat(a.Path); err == nil {
				if err := os.Chtimes(a.Path, now, now); err != nil {
					return "", err
				}
				return "touched " + a.Path, nil
			}
			f, err := os.OpenFile(a.Path, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return "", err
			}
			f.Close()
			return "created " + a.Path, nil
		},
	}
}

// capMatches bounds grep-style output to protect the model's context window,
// truncating to a line and byte budget with a footer noting how much was cut.
func capMatches(text string) string {
	const maxLines, maxBytes = 100, 10 * 1024
	lines := strings.Split(text, "\n")
	truncatedBy := 0
	if len(lines) > maxLines {
		truncatedBy = len(lines) - maxLines
		lines = lines[:maxLines]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxBytes {
		out = out[:maxBytes]
		if i := strings.LastIndexByte(out, '\n'); i > 0 {
			out = out[:i]
		}
		if truncatedBy == 0 {
			truncatedBy = -1 // signal byte-based truncation
		}
	}
	switch {
	case truncatedBy > 0:
		out += fmt.Sprintf("\n\n... and %d more matching line(s) (truncated; narrow the pattern or path)", truncatedBy)
	case truncatedBy < 0:
		out += "\n\n... (truncated at 10KB; narrow the pattern or path)"
	}
	return out
}

func GrepTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "grep",
			Description: "Search for a regex pattern in files. Returns matching lines prefixed with file:line. ANSI color codes are stripped from output. Use this to find code patterns, usages, TODOs, or any text across the project.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"pattern":     {Type: "string", Description: "Regex pattern to search for."},
					"path":        {Type: "string", Description: "File or directory to search. Defaults to '.'."},
					"recursive":   {Type: "boolean", Description: "Search directories recursively. Defaults to true when path is a directory."},
					"ignore_case": {Type: "boolean", Description: "Case-insensitive match."},
					"file_types":  {Type: "string", Description: "Comma-separated file extensions to include (e.g. '.go,.md')."},
				},
				Required: []string{"pattern"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Pattern    string `json:"pattern"`
				Path       string `json:"path"`
				Recursive  *bool  `json:"recursive"`
				IgnoreCase bool   `json:"ignore_case"`
				FileTypes  string `json:"file_types"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			path := a.Path
			if path == "" {
				path = "."
			}
			recursive := false
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				recursive = true
			}
			if a.Recursive != nil {
				recursive = *a.Recursive
			}
			argv := []string{"-nE", "--color=never"}
			if a.IgnoreCase {
				argv = append(argv, "-i")
			}
			if recursive {
				argv = append(argv, "-r")
			}
			if a.FileTypes != "" {
				for _, ft := range strings.Split(a.FileTypes, ",") {
					ft = strings.TrimSpace(ft)
					if ft != "" {
						argv = append(argv, "--include="+ft)
					}
				}
			}
			argv = append(argv, "--", a.Pattern, path)
			cmd := exec.CommandContext(ctx, "grep", argv...)
			out, err := cmd.CombinedOutput()
			text := strings.TrimRight(stripANSI(string(out)), "\n")
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
					return "no matches", nil
				}
				if text != "" {
					return capMatches(text), nil
				}
				return "", err
			}
			if text == "" {
				return "no matches", nil
			}
			return capMatches(text), nil
		},
	}
}

func RunShellTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "run_shell",
			Description: "Run a shell command via `sh -c`. Use for awk, sed, find, complex pipelines, or anything not covered by a dedicated tool. Returns combined stdout+stderr. Supports stdin input via the stdin parameter. Non-zero exits are reported in the result. Default timeout 30s, max 300s.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"command":     {Type: "string", Description: "The shell command to execute."},
					"working_dir": {Type: "string", Description: "Directory to run in. Defaults to the current working directory."},
					"timeout_sec": {Type: "number", Description: "Hard timeout in seconds. Defaults to 30, max 300."},
					"stdin":       {Type: "string", Description: "Text to pipe into the command's standard input."},
				},
				Required: []string{"command"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Command    string  `json:"command"`
				WorkingDir string  `json:"working_dir"`
				TimeoutSec float64 `json:"timeout_sec"`
				Stdin      string  `json:"stdin"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if strings.TrimSpace(a.Command) == "" {
				return "", fmt.Errorf("command is required")
			}
			timeout := 30 * time.Second
			if a.TimeoutSec > 0 {
				timeout = time.Duration(a.TimeoutSec * float64(time.Second))
			}
			if timeout > 300*time.Second {
				timeout = 300 * time.Second
			}
			return runShellCommand(ctx, a.Command, a.WorkingDir, a.Stdin, timeout)
		},
	}
}

func AppendFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "append_file",
			Description: "Append text to the end of a file. Creates the file (and parent directories) if missing.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":    {Type: "string", Description: "Absolute or relative path to the file."},
					"content": {Type: "string", Description: "Text to append. Add a trailing newline yourself if you want one."},
				},
				Required: []string{"path", "content"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
				return "", err
			}
			f, err := os.OpenFile(a.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return "", err
			}
			defer f.Close()
			if _, err := f.WriteString(a.Content); err != nil {
				return "", err
			}
			f.Close()
			hash, _ := calculateHash(a.Path)
			return fmt.Sprintf("appended %d bytes to %s\nNew Hash: %s", len(a.Content), a.Path, hash), nil
		},
	}
}

// leadingWS returns the leading run of spaces/tabs in s.
func leadingWS(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// reindentBlock rebases the indentation of newStr from oldIndent to fileIndent,
// preserving each line's relative indentation. Used when a whitespace-normalized
// match applies the model's replacement to the file's actual indentation.
func reindentBlock(newStr, oldIndent, fileIndent string) string {
	if oldIndent == fileIndent {
		return newStr
	}
	lines := strings.Split(newStr, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		lines[i] = fileIndent + strings.TrimPrefix(ln, oldIndent)
	}
	return strings.Join(lines, "\n")
}

// applyEdit replaces oldStr with newStr in content using a tiered matcher:
//
//	tier 1: exact substring match (whitespace must match exactly)
//	tier 2: whitespace-normalized, line-based match (tolerates indentation and
//	        CRLF/LF differences; re-indents newStr to the file's real indentation)
//
// It returns the updated content, the number of replacements, and the tier that
// matched. Tier 2 is only attempted when tier 1 finds nothing.
func applyEdit(content, oldStr, newStr string, replaceAll bool) (updated string, count, tier int, err error) {
	// Tier 1: exact.
	if c := strings.Count(content, oldStr); c > 0 {
		if c > 1 && !replaceAll {
			return "", c, 1, fmt.Errorf("old_string appears %d times; pass replace_all=true or use a more specific snippet", c)
		}
		if replaceAll {
			return strings.ReplaceAll(content, oldStr, newStr), c, 1, nil
		}
		return strings.Replace(content, oldStr, newStr, 1), c, 1, nil
	}

	// Tier 2: whitespace-normalized line match.
	crlf := strings.Count(content, "\r\n")
	useCRLF := crlf > (strings.Count(content, "\n") - crlf)
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	oldNorm := strings.ReplaceAll(oldStr, "\r\n", "\n")
	newNorm := strings.ReplaceAll(newStr, "\r\n", "\n")

	cLines := strings.Split(norm, "\n")
	oLines := strings.Split(oldNorm, "\n")
	for len(oLines) > 1 && strings.TrimSpace(oLines[len(oLines)-1]) == "" {
		oLines = oLines[:len(oLines)-1]
	}
	k := len(oLines)
	if k == 0 {
		return "", 0, 2, fmt.Errorf("old_string is empty")
	}

	// Find all window start indices whose trimmed lines match oLines exactly.
	var starts []int
	if k <= len(cLines) {
		for i := 0; i+k <= len(cLines); i++ {
			match := true
			for j := 0; j < k; j++ {
				if strings.TrimSpace(cLines[i+j]) != strings.TrimSpace(oLines[j]) {
					match = false
					break
				}
			}
			if match {
				starts = append(starts, i)
			}
		}
	}

	if len(starts) == 0 {
		// Tier 3: fuzzy line-block match.
		return fuzzyEdit(cLines, oLines, newNorm, useCRLF)
	}
	if len(starts) > 1 && !replaceAll {
		return "", len(starts), 2, fmt.Errorf("old_string matches %d locations (after whitespace-normalization); pass replace_all=true or use a more specific snippet", len(starts))
	}

	// Apply replacements from the bottom up so earlier indices stay valid.
	out := append([]string(nil), cLines...)
	for idx := len(starts) - 1; idx >= 0; idx-- {
		i := starts[idx]
		fileIndent := leadingWS(out[i])
		oldIndent := leadingWS(oLines[0])
		repl := strings.Split(reindentBlock(newNorm, oldIndent, fileIndent), "\n")
		out = append(out[:i], append(repl, out[i+k:]...)...)
		if !replaceAll {
			break
		}
	}
	joined := strings.Join(out, "\n")
	if useCRLF {
		joined = strings.ReplaceAll(joined, "\n", "\r\n")
	}
	return joined, len(starts), 2, nil
}

// Tier-3 fuzzy thresholds: a match must be both strong in absolute terms and an
// unambiguous winner over any other (non-overlapping) region.
const (
	fuzzyAccept = 0.85
	fuzzyMargin = 0.10
)

// fuzzyEdit performs a similarity-based single-best-window replacement. It only
// commits when the best window scores >= fuzzyAccept AND beats the next-best
// non-overlapping window by >= fuzzyMargin; otherwise it refuses and returns the
// closest region so the model can copy the exact current text.
func fuzzyEdit(cLines, oLines []string, newNorm string, useCRLF bool) (string, int, int, error) {
	k := len(oLines)
	bestScore, bestStart := -1.0, -1
	for i := 0; i+k <= len(cLines); i++ {
		if s := windowScore(cLines, oLines, i); s > bestScore {
			bestScore, bestStart = s, i
		}
	}
	if bestStart < 0 {
		return "", 0, 3, fmt.Errorf("old_string not found")
	}
	secondScore := -1.0
	for i := 0; i+k <= len(cLines); i++ {
		if absInt(i-bestStart) < k {
			continue // overlaps the best window
		}
		if s := windowScore(cLines, oLines, i); s > secondScore {
			secondScore = s
		}
	}
	if bestScore < fuzzyAccept || (secondScore >= 0 && bestScore-secondScore < fuzzyMargin) {
		region := strings.Join(cLines[bestStart:bestStart+k], "\n")
		return "", 0, 3, fmt.Errorf("no confident match for old_string (best similarity %.2f at lines %d-%d). Closest current text:\n%s\nCopy it exactly (whitespace included) and retry", bestScore, bestStart+1, bestStart+k, region)
	}

	out := append([]string(nil), cLines...)
	fileIndent := leadingWS(out[bestStart])
	oldIndent := leadingWS(oLines[0])
	repl := strings.Split(reindentBlock(newNorm, oldIndent, fileIndent), "\n")
	out = append(out[:bestStart], append(repl, out[bestStart+k:]...)...)
	joined := strings.Join(out, "\n")
	if useCRLF {
		joined = strings.ReplaceAll(joined, "\n", "\r\n")
	}
	return joined, 1, 3, nil
}

// windowScore is the mean per-line similarity of the k-line window of cLines
// starting at start against oLines.
func windowScore(cLines, oLines []string, start int) float64 {
	sum := 0.0
	for j := range oLines {
		sum += lineSimilarity(cLines[start+j], oLines[j])
	}
	return sum / float64(len(oLines))
}

// lineSimilarity is 1 - normalized Levenshtein distance over trimmed lines.
func lineSimilarity(a, b string) float64 {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return 1
	}
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	if maxLen == 0 {
		return 1
	}
	return 1 - float64(levenshtein(a, b))/float64(maxLen)
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func EditFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "edit_file",
			Description: "Replace a text snippet inside a file. Matching is exact first; if that fails it falls back to whitespace/indentation-tolerant line matching. By default old_string must resolve to exactly one location (safety). Set replace_all to true to substitute every occurrence. Use this for incremental edits instead of rewriting the whole file with write_file.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":        {Type: "string", Description: "Path to the file."},
					"old_string":  {Type: "string", Description: "Text currently in the file. Optional if start_line/end_line are specified. Exact whitespace is preferred but indentation differences are tolerated."},
					"new_string":  {Type: "string", Description: "Replacement text."},
					"replace_all": {Type: "boolean", Description: "Replace every occurrence (only applies when using old_string). Default false."},
					"start_line":  {Type: "number", Description: "Optional first line to replace (1-indexed, inclusive). Use instead of old_string for precise edits."},
					"end_line":    {Type: "number", Description: "Optional last line to replace (1-indexed, inclusive). Use instead of old_string for precise edits."},
				},
				Required: []string{"path", "new_string"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
				StartLine  int    `json:"start_line"`
				EndLine    int    `json:"end_line"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}

			var updated string
			var count int
			var tier int

			if a.StartLine != 0 || a.EndLine != 0 {
				if a.StartLine < 1 || a.EndLine < 1 {
					return "", fmt.Errorf("start_line and end_line must be 1-indexed (got start_line=%d, end_line=%d)", a.StartLine, a.EndLine)
				}
				lines := strings.Split(string(data), "\n")
				if a.StartLine > len(lines) || a.EndLine > len(lines) {
					return "", fmt.Errorf("start_line %d or end_line %d exceeds file length %d", a.StartLine, a.EndLine, len(lines))
				}
				if a.StartLine > a.EndLine {
					return "", fmt.Errorf("start_line %d is greater than end_line %d", a.StartLine, a.EndLine)
				}
				var newLines []string
				newLines = append(newLines, lines[:a.StartLine-1]...)
				newLines = append(newLines, a.NewString)
				newLines = append(newLines, lines[a.EndLine:]...)
				updated = strings.Join(newLines, "\n")
				count = 1
				tier = 1
			} else {
				if a.OldString == "" {
					return "", fmt.Errorf("old_string is required when start_line/end_line are not specified (use write_file to replace or write new content)")
				}
				var editErr error
				updated, count, tier, editErr = applyEdit(string(data), a.OldString, a.NewString, a.ReplaceAll)
				if editErr != nil {
					if tier >= 2 {
						return "", fmt.Errorf("%w in %s", editErr, a.Path)
					}
					return "", fmt.Errorf("%s: %w. If matching fails, consider reading the file with line numbers and using start_line/end_line for precise editing.", a.Path, editErr)
				}
			}

			// Verify-before-write: if the file parsed cleanly before the edit,
			// reject (don't write) an edit that would break its syntax.
			if verifyBytes(a.Path, data) == nil {
				if verr := verifyBytes(a.Path, []byte(updated)); verr != nil {
					return "", fmt.Errorf("edit rejected: it would introduce a syntax error in %s: %v\nNo changes were written — fix start_line/end_line/new_string and retry", a.Path, verr)
				}
			}
			info, err := os.Stat(a.Path)
			mode := os.FileMode(0o644)
			if err == nil {
				mode = info.Mode().Perm()
			}
			if err := os.WriteFile(a.Path, []byte(updated), mode); err != nil {
				return "", err
			}
			hash, _ := calculateHash(a.Path)
			tierNote := ""
			if a.StartLine != 0 || a.EndLine != 0 {
				tierNote = fmt.Sprintf(" (replaced lines %d to %d)", a.StartLine, a.EndLine)
			} else {
				switch tier {
				case 2:
					tierNote = " (matched after whitespace-normalization; copy exact text next time for precision)"
				case 3:
					tierNote = " (matched by fuzzy similarity — verify the diff carefully)"
				}
			}
			result := fmt.Sprintf("edited %s: replaced %d occurrence(s)%s\nNew Hash: %s", a.Path, count, tierNote, hash)
			if diff := unifiedDiff(string(data), updated, a.Path); diff != "" {
				result += "\n" + diff
			}
			return result, nil
		},
	}
}

func DeleteFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "delete_file",
			Description: "Delete a file. To remove a directory and its contents, set recursive=true.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":      {Type: "string", Description: "Path to delete."},
					"recursive": {Type: "boolean", Description: "Required when deleting a non-empty directory."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path      string `json:"path"`
				Recursive bool   `json:"recursive"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if a.Recursive {
				if err := os.RemoveAll(a.Path); err != nil {
					return "", err
				}
				return "removed " + a.Path + " (recursive)", nil
			}
			if err := os.Remove(a.Path); err != nil {
				return "", err
			}
			return "removed " + a.Path, nil
		},
	}
}

func MoveFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "move_file",
			Description: "Move or rename a file or directory.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"source":      {Type: "string", Description: "Current path."},
					"destination": {Type: "string", Description: "New path. Parent directories are created if needed."},
				},
				Required: []string{"source", "destination"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Source      string `json:"source"`
				Destination string `json:"destination"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Source == "" || a.Destination == "" {
				return "", fmt.Errorf("source and destination are required")
			}
			if err := os.MkdirAll(filepath.Dir(a.Destination), 0o755); err != nil {
				return "", err
			}
			if err := os.Rename(a.Source, a.Destination); err != nil {
				return "", err
			}
			return fmt.Sprintf("moved %s -> %s", a.Source, a.Destination), nil
		},
	}
}

func CopyFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "copy_file",
			Description: "Copy a file or directory recursively. Uses cp -r for directories. Parent directories are created automatically for the destination.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"source":      {Type: "string", Description: "Source file or directory path."},
					"destination": {Type: "string", Description: "Destination path. Parent directories are created if needed."},
				},
				Required: []string{"source", "destination"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Source      string `json:"source"`
				Destination string `json:"destination"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Source == "" || a.Destination == "" {
				return "", fmt.Errorf("source and destination are required")
			}
			if err := os.MkdirAll(filepath.Dir(a.Destination), 0o755); err != nil {
				return "", err
			}
			cmd := exec.CommandContext(ctx, "cp", "-r", a.Source, a.Destination)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return fmt.Sprintf("copied %s -> %s", a.Source, a.Destination), nil
		},
	}
}

func FindFilesTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "find_files",
			Description: "Walk a directory tree and return paths whose basename matches a glob pattern (e.g. '*.go', 'README*'). Skips .git and node_modules by default.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"pattern":        {Type: "string", Description: "Glob pattern matched against the basename. Use '*' for anything."},
					"path":           {Type: "string", Description: "Root directory to search. Defaults to '.'."},
					"max_depth":      {Type: "number", Description: "Maximum directory depth (0 = root only). Default 10."},
					"include_hidden": {Type: "boolean", Description: "Descend into dot-directories (default false). .git/node_modules stay skipped."},
				},
				Required: []string{"pattern"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Pattern       string `json:"pattern"`
				Path          string `json:"path"`
				MaxDepth      *int   `json:"max_depth"`
				IncludeHidden bool   `json:"include_hidden"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			root := a.Path
			if root == "" {
				root = "."
			}
			maxDepth := 10
			if a.MaxDepth != nil {
				maxDepth = *a.MaxDepth
			}
			rootDepth := strings.Count(filepath.Clean(root), string(filepath.Separator))
			gi := gitignore.NewMatcher(root)
			var matches []string
			err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				if p != root && gi.IsIgnored(p) {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				name := info.Name()
				if !a.IncludeHidden && strings.HasPrefix(name, ".") && p != root {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				depth := strings.Count(filepath.Clean(p), string(filepath.Separator)) - rootDepth
				if depth > maxDepth {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if !info.IsDir() {
					if ok, _ := filepath.Match(a.Pattern, name); ok {
						matches = append(matches, p)
					}
				}
				return nil
			})
			if err != nil {
				return "", err
			}
			if len(matches) == 0 {
				return "no matches", nil
			}
			sort.Strings(matches)
			return joinLines(matches), nil
		},
	}
}

func FileInfoTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "file_info",
			Description: "Return metadata for a path: type (file/dir/symlink), size, permissions, modtime. Errors if the path does not exist.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to inspect."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			info, err := os.Lstat(a.Path)
			if err != nil {
				return "", err
			}
			kind := "file"
			switch {
			case info.IsDir():
				kind = "dir"
			case info.Mode()&os.ModeSymlink != 0:
				kind = "symlink"
			}
			return fmt.Sprintf("path: %s\ntype: %s\nsize: %d\nmode: %s\nmodtime: %s",
				a.Path, kind, info.Size(), info.Mode().String(), info.ModTime().Format(time.RFC3339)), nil
		},
	}
}

func GetWorkingDirectoryTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "get_working_directory",
			Description: "Return the absolute path of the process's current working directory. Use this when you need to know where relative paths resolve.",
			Parameters:  Schema{Type: "object", Properties: map[string]Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			cwd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			return cwd, nil
		},
	}
}

func WebFetchTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "web_fetch",
			Description: "Fetch a web page and return its content as clean plain text. HTML pages are converted to readable text — scripts, styles, and markup are stripped. Use this to read documentation, blog posts, or any web content. Returns the HTTP status code followed by the extracted text.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"url":       {Type: "string", Description: "The URL to fetch."},
					"max_chars": {Type: "number", Description: "Maximum characters to return. Default 25000."},
				},
				Required: []string{"url"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				URL      string `json:"url"`
				MaxChars int    `json:"max_chars"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.URL == "" {
				return "", fmt.Errorf("url is required")
			}
			if a.MaxChars <= 0 {
				a.MaxChars = 25000
			}

			resp, err := webReq(ctx, "GET", a.URL, nil)
			if err != nil {
				return "", fmt.Errorf("fetch failed: %w", err)
			}
			defer resp.Body.Close()

			limited := io.LimitReader(resp.Body, int64(a.MaxChars*5)) // headroom for HTML bloat
			ct := resp.Header.Get("Content-Type")

			var text string
			if isPlainText(ct) {
				raw, _ := io.ReadAll(limited)
				text = string(raw)
			} else {
				text = htmlToText(limited, a.MaxChars)
			}

			if text == "" {
				return fmt.Sprintf("[status %d] (no extractable text)", resp.StatusCode), nil
			}
			if len(text) > a.MaxChars {
				text = text[:a.MaxChars]
			}

			return fmt.Sprintf("[status %d]\n%s", resp.StatusCode, collapseWhitespace(text)), nil
		},
	}
}

func GetProjectTreeTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "get_project_tree",
			Description: "Return an ASCII tree of the project directory with proper branch markers (├── / └──). Directories are suffixed with '/'. Skipped dirs like .git/node_modules show a count note. Capped at max_depth and max_entries to stay readable.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":        {Type: "string", Description: "Root directory. Defaults to '.'."},
					"max_depth":   {Type: "number", Description: "Maximum directory depth. Default 4, max 8."},
					"max_entries": {Type: "number", Description: "Maximum entries to output. Default 200."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path       string `json:"path"`
				MaxDepth   int    `json:"max_depth"`
				MaxEntries int    `json:"max_entries"`
			}
			json.Unmarshal(args, &a)
			root := a.Path
			if root == "" {
				root = "."
			}
			if a.MaxDepth <= 0 {
				a.MaxDepth = 4
			}
			if a.MaxDepth > 8 {
				a.MaxDepth = 8
			}
			if a.MaxEntries <= 0 {
				a.MaxEntries = 200
			}

			type entry struct {
				rel     string
				isDir   bool
				skipped bool
			}
			gi := gitignore.NewMatcher(root)
			var entries []entry
			skippedDirs := 0
			rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))

			filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err != nil || len(entries) >= a.MaxEntries {
					return nil
				}
				rel, _ := filepath.Rel(root, path)
				if rel == "." {
					return nil
				}
				if path != root && gi.IsIgnored(path) {
					if d.IsDir() {
						skippedDirs++
						return filepath.SkipDir
					}
					return nil
				}
				depth := strings.Count(filepath.Clean(path), string(os.PathSeparator)) - rootDepth
				if depth > a.MaxDepth {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				entries = append(entries, entry{rel: rel, isDir: d.IsDir()})
				return nil
			})

			var b strings.Builder
			b.WriteString(filepath.Clean(root))
			if !strings.HasSuffix(root, "/") {
				b.WriteByte('/')
			}
			b.WriteByte('\n')

			for i, e := range entries {
				depth := strings.Count(e.rel, string(os.PathSeparator))
				name := filepath.Base(e.rel)
				isLast := i == len(entries)-1
				if !isLast && i+1 < len(entries) {
					nextDepth := strings.Count(entries[i+1].rel, string(os.PathSeparator))
					if nextDepth < depth {
						isLast = true
					} else if nextDepth == depth {
						isLast = false
					} else {
						// Check if any later sibling at same level
						isLast = true
						for j := i + 1; j < len(entries); j++ {
							nextD := strings.Count(entries[j].rel, string(os.PathSeparator))
							if nextD < depth {
								break
							}
							if nextD == depth {
								isLast = false
								break
							}
						}
					}
				}

				// Build prefix with proper │ and ├── / └──
				parts := strings.Split(e.rel, string(os.PathSeparator))
				for d := 0; d < depth; d++ {
					// Check if the ancestor at this depth was last
					ancestorLast := true
					ancestorPath := strings.Join(parts[:d+1], string(os.PathSeparator))
					for j := i + 1; j < len(entries); j++ {
						if strings.HasPrefix(entries[j].rel, ancestorPath+string(os.PathSeparator)) {
							ancestorLast = false
							break
						}
						if !strings.HasPrefix(entries[j].rel, strings.Join(parts[:d+1], string(os.PathSeparator))) {
							break
						}
					}
					// Simpler: check if any subsequent entry is at this depth or deeper under this ancestor
					for j := i + 1; j < len(entries); j++ {
						jParts := strings.Split(entries[j].rel, string(os.PathSeparator))
						if len(jParts) <= d {
							break
						}
						if jParts[d] != parts[d] {
							break
						}
						ancestorLast = false
						break
					}
					if ancestorLast {
						b.WriteString("   ")
					} else {
						b.WriteString("│  ")
					}
				}

				if isLast {
					b.WriteString("└── ")
				} else {
					b.WriteString("├── ")
				}
				if e.isDir {
					b.WriteString(name + "/")
				} else {
					b.WriteString(name)
				}
				b.WriteByte('\n')
			}

			if skippedDirs > 0 {
				fmt.Fprintf(&b, "(%d build/vcs directories skipped)\n", skippedDirs)
			}
			if len(entries) >= a.MaxEntries {
				fmt.Fprintf(&b, "(output capped at %d entries)\n", a.MaxEntries)
			}
			fmt.Fprintf(&b, "(%d entries total)", len(entries))
			return b.String(), nil
		},
	}
}

func AskUserTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "ask_user",
			Description: "Ask the user a question when you need clarification before proceeding. Use this for: confirming destructive operations, choosing between multiple approaches, getting missing context, or when you're stuck. Include clear options in the question to make it easy for the user to answer. After calling this, STOP and wait — the user's next message will contain their answer.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"question": {Type: "string", Description: "The question to ask the user. Be specific and include context so they can give a quick answer."},
					"options":  {Type: "string", Description: "Optional: list of suggested answers separated by '|' (e.g. 'yes|no|show me an example')."},
				},
				Required: []string{"question"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Question string `json:"question"`
				Options  string `json:"options"`
			}
			json.Unmarshal(args, &a)
			msg := "QUESTION: " + a.Question
			if a.Options != "" {
				msg += "\nOptions: [" + a.Options + "]"
			}
			msg += "\n\n(Stop here and wait for the user to answer before continuing.)"
			return msg, nil
		},
	}
}

func GitStatusTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_status",
			Description: "Show full git status: current branch, upstream tracking, staged/unstaged/untracked changes, and stash count. Use this to understand repo state before any git operations.",
			Parameters:  Schema{Type: "object", Properties: map[string]Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var out strings.Builder

			// Branch info
			cmd := exec.CommandContext(ctx, "git", "branch", "--show-current")
			b, _ := cmd.Output()
			branch := strings.TrimSpace(string(b))
			out.WriteString("branch: " + branch + "\n")

			// Tracking
			cmd = exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "@{upstream}")
			b, err := cmd.Output()
			if err == nil {
				out.WriteString("upstream: " + strings.TrimSpace(string(b)) + "\n")
			}

			// Ahead/behind
			cmd = exec.CommandContext(ctx, "git", "status", "-sb")
			b, _ = cmd.Output()
			sb := strings.TrimSpace(string(b))
			if idx := strings.IndexByte(sb, '\n'); idx >= 0 {
				sb = sb[idx+1:]
			}
			out.WriteString("status: " + sb + "\n")

			// Stash count
			cmd = exec.CommandContext(ctx, "git", "stash", "list")
			b, _ = cmd.Output()
			stashLines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(stashLines) > 0 && stashLines[0] != "" {
				out.WriteString("stashes: " + strconv.Itoa(len(stashLines)) + "\n")
			}

			// Short status
			out.WriteString("\nchanges:\n")
			cmd = exec.CommandContext(ctx, "git", "status", "-s")
			b, err = cmd.Output()
			if err != nil {
				return "", err
			}
			if len(b) == 0 {
				out.WriteString("(working tree clean)\n")
			} else {
				out.Write(b)
			}

			return out.String(), nil
		},
	}
}

func GitDiffTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_diff",
			Description: "Show git diff. By default shows unstaged changes. Use staged=true for staged changes. Use from_commit/to_commit to compare specific commits. Use path to filter to specific files or directories.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"staged":      {Type: "boolean", Description: "Show staged changes instead of unstaged."},
					"from_commit": {Type: "string", Description: "Base commit/branch for comparison (e.g. 'HEAD~3' or 'main')."},
					"to_commit":   {Type: "string", Description: "Target commit/branch. Defaults to working tree."},
					"path":        {Type: "string", Description: "Restrict diff to this file or directory."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Staged     bool   `json:"staged"`
				FromCommit string `json:"from_commit"`
				ToCommit   string `json:"to_commit"`
				Path       string `json:"path"`
			}
			json.Unmarshal(args, &a)
			argv := []string{"diff", "--color=never"}
			if a.Staged {
				argv = append(argv, "--staged")
			}
			if a.FromCommit != "" {
				if a.ToCommit != "" {
					argv = append(argv, a.FromCommit+"..."+a.ToCommit)
				} else {
					argv = append(argv, a.FromCommit)
				}
			}
			if a.Path != "" {
				argv = append(argv, "--", a.Path)
			}
			cmd := exec.CommandContext(ctx, "git", argv...)
			out, err := cmd.CombinedOutput()
			text := strings.TrimRight(string(out), "\n")
			if err != nil {
				if text != "" {
					return text, nil
				}
				return "", err
			}
			if text == "" {
				return "(no changes)", nil
			}
			return text, nil
		},
	}
}

func GitLogTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_log",
			Description: "Show git commit history with graph, author, and date. Filter by author, path, or date range. Use this to understand project history and find specific changes.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"count":  {Type: "number", Description: "Number of commits to show. Default 10, max 50."},
					"author": {Type: "string", Description: "Filter by author name or email."},
					"path":   {Type: "string", Description: "Only show commits touching this file/directory."},
					"since":  {Type: "string", Description: "Show commits more recent than this date (e.g. '2024-01-01' or '2 weeks ago')."},
					"grep":   {Type: "string", Description: "Filter commits whose message matches this pattern."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Count  int    `json:"count"`
				Author string `json:"author"`
				Path   string `json:"path"`
				Since  string `json:"since"`
				Grep   string `json:"grep"`
			}
			json.Unmarshal(args, &a)
			if a.Count <= 0 {
				a.Count = 10
			}
			if a.Count > 50 {
				a.Count = 50
			}

			argv := []string{"log", "-n", strconv.Itoa(a.Count), "--graph", "--color=never", "--format=%h %ad %an: %s", "--date=short"}
			if a.Author != "" {
				argv = append(argv, "--author="+a.Author)
			}
			if a.Since != "" {
				argv = append(argv, "--since="+a.Since)
			}
			if a.Grep != "" {
				argv = append(argv, "--grep="+a.Grep)
			}
			if a.Path != "" {
				argv = append(argv, "--", a.Path)
			}
			cmd := exec.CommandContext(ctx, "git", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", err
			}
			return string(out), nil
		},
	}
}

func GitAddTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_add",
			Description: "Add file contents to the git index (git add).",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"paths": {Type: "string", Description: "File or directory paths to add. Use '.' for all."},
				},
				Required: []string{"paths"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Paths string `json:"paths"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			cmd := exec.CommandContext(ctx, "git", "add", a.Paths)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return "added " + a.Paths, nil
		},
	}
}

func GitCommitTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_commit",
			Description: "Record changes to the git repository. Use all=true to auto-stage all modified/deleted files. Use amend=true to amend the previous commit (keep same message or provide a new one). Without flags, commits only previously staged changes.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"message": {Type: "string", Description: "The commit message."},
					"all":     {Type: "boolean", Description: "Automatically stage all modified and deleted files before committing."},
					"amend":   {Type: "boolean", Description: "Amend the previous commit instead of creating a new one."},
				},
				Required: []string{"message"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Message string `json:"message"`
				All     bool   `json:"all"`
				Amend   bool   `json:"amend"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			argv := []string{"commit", "-m", a.Message}
			if a.All {
				argv = append(argv, "--all")
			}
			if a.Amend {
				argv = append(argv, "--amend")
			}
			cmd := exec.CommandContext(ctx, "git", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return string(out), nil
		},
	}
}

func WebSearchTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "web_search",
			Description: "Search the web for a query using DuckDuckGo. Returns numbered results with title, snippet, and URL. Use this to find current information, documentation, or anything you don't already know. Be specific in your query for best results.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"query":       {Type: "string", Description: "The search query. Include keywords, version numbers, or dates for better results."},
					"max_results": {Type: "number", Description: "Maximum number of results. Default 10, max 20."},
				},
				Required: []string{"query"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Query      string `json:"query"`
				MaxResults int    `json:"max_results"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Query == "" {
				return "", fmt.Errorf("query is required")
			}
			if a.MaxResults <= 0 {
				a.MaxResults = 10
			}
			if a.MaxResults > 20 {
				a.MaxResults = 20
			}

			queryURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(a.Query)
			resp, err := webReq(ctx, "GET", queryURL, nil)
			if err != nil {
				return "", fmt.Errorf("search failed: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
				return "", fmt.Errorf("search returned status %d: %s", resp.StatusCode, string(body))
			}

			results := parseDDG(resp.Body, a.MaxResults)
			if len(results) == 0 {
				return "no results found for query: " + a.Query, nil
			}
			return strings.Join(results, "\n"), nil
		},
	}
}

func FindSymbolTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "find_symbol",
			Description: "Search for a symbol definition across the project. Matches Go func/method/type/var/const, Rust fn/struct/trait/enum/impl/type, Python class/def, C/C++ functions, Java/TS classes, and more. Use this to locate where things are defined.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"symbol":     {Type: "string", Description: "The name of the symbol to find."},
					"file_types": {Type: "string", Description: "Optional comma-separated extensions to filter by (e.g. '.go,.rs,.py')."},
				},
				Required: []string{"symbol"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Symbol    string `json:"symbol"`
				FileTypes string `json:"file_types"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Symbol == "" {
				return "", fmt.Errorf("symbol is required")
			}

			// Broad pattern covering many languages:
			// Go: func, func (receiver), type, var, const
			// Rust: fn, struct, trait, enum, impl, type, const, static
			// Python: class, def, async def, @decorators
			// C/C++/Java/TS/JS: function declarations, class, interface, enum
			// Zig: fn, const, var, struct, enum, union
			sym := regexp.QuoteMeta(a.Symbol)
			pattern := fmt.Sprintf(
				`(^|[[:space:]])(func|fn|class|struct|trait|enum|impl|interface|type|var|const|def|let|static|async[[:space:]]+def|union|module|pub)[[:space:]]+.*%s|^[[:space:]]*%s[[:space:]]*:=|^[[:space:]]*%s[[:space:]]*=`,
				sym, sym, sym,
			)
			argv := []string{"-rnE", "--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=build", "--exclude-dir=vendor", "--exclude-dir=target", pattern}
			if a.FileTypes != "" {
				argv = append(argv, "--include="+a.FileTypes)
			}
			argv = append(argv, ".")
			cmd := exec.CommandContext(ctx, "grep", argv...)
			out, _ := cmd.CombinedOutput()
			text := filterCodeMatches(strings.TrimSpace(stripANSI(string(out))), 100)
			if text == "" {
				return fmt.Sprintf("symbol %q not found in project", a.Symbol), nil
			}
			return text, nil
		},
	}
}

func GitBranchTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_branch",
			Description: "List, create, or delete git branches. With no arguments, lists all local branches (current marked with *). Use action='create' with a name to create a new branch. Use action='delete' to delete a branch (safe — refuses if not merged). Use remote=true to list remote branches.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"action": {Type: "string", Description: "Action: 'list' (default), 'create', or 'delete'."},
					"name":   {Type: "string", Description: "Branch name (required for create/delete)."},
					"remote": {Type: "boolean", Description: "List remote-tracking branches instead of local branches."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Action string `json:"action"`
				Name   string `json:"name"`
				Remote bool   `json:"remote"`
			}
			json.Unmarshal(args, &a)
			if a.Action == "" {
				a.Action = "list"
			}
			switch a.Action {
			case "create":
				if a.Name == "" {
					return "", fmt.Errorf("name is required for create")
				}
				cmd := exec.CommandContext(ctx, "git", "branch", a.Name)
				out, err := cmd.CombinedOutput()
				if err != nil {
					return string(out), err
				}
				return "created branch " + a.Name, nil
			case "delete":
				if a.Name == "" {
					return "", fmt.Errorf("name is required for delete")
				}
				cmd := exec.CommandContext(ctx, "git", "branch", "-d", a.Name)
				out, err := cmd.CombinedOutput()
				if err != nil {
					return string(out), err
				}
				return string(out), nil
			default:
				argv := []string{"branch"}
				if a.Remote {
					argv = append(argv, "-r")
				}
				cmd := exec.CommandContext(ctx, "git", argv...)
				out, _ := cmd.CombinedOutput()
				return string(out), nil
			}
		},
	}
}

func GitCheckoutTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_checkout",
			Description: "Switch to a branch, create-and-switch to a new one, or restore files. Use new_branch=true with target to create a branch and switch to it. Use target with a file path to restore that file from HEAD.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"target":     {Type: "string", Description: "Branch name to switch to, or file path to restore."},
					"new_branch": {Type: "boolean", Description: "Create a new branch with the given target name before switching."},
				},
				Required: []string{"target"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Target    string `json:"target"`
				NewBranch bool   `json:"new_branch"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Target == "" {
				return "", fmt.Errorf("target is required")
			}
			argv := []string{"checkout"}
			if a.NewBranch {
				argv = append(argv, "-b")
			}
			argv = append(argv, a.Target)
			cmd := exec.CommandContext(ctx, "git", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return string(out), nil
		},
	}
}

func GitPullTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_pull",
			Description: "Fetch from and integrate with a remote repository. Uses rebase by default for a clean linear history. Specify remote and branch, or defaults to the current upstream.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"remote": {Type: "string", Description: "Remote name. Defaults to 'origin'."},
					"branch": {Type: "string", Description: "Remote branch. Defaults to current tracking branch."},
					"rebase": {Type: "boolean", Description: "Use rebase instead of merge. Default true."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Remote string `json:"remote"`
				Branch string `json:"branch"`
				Rebase *bool  `json:"rebase"`
			}
			json.Unmarshal(args, &a)
			if a.Remote == "" {
				a.Remote = "origin"
			}
			rebase := true
			if a.Rebase != nil {
				rebase = *a.Rebase
			}
			argv := []string{"pull"}
			if rebase {
				argv = append(argv, "--rebase")
			}
			if a.Branch != "" {
				argv = append(argv, a.Remote, a.Branch)
			} else {
				argv = append(argv, a.Remote)
			}
			cmd := exec.CommandContext(ctx, "git", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return string(out), nil
		},
	}
}

func GitPushTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_push",
			Description: "Push commits to a remote repository. By default pushes current branch to the same-named remote branch. Use set_upstream=true on first push of a new branch. Uses --force-with-lease for safer force push.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"remote":       {Type: "string", Description: "Remote name. Defaults to 'origin'."},
					"branch":       {Type: "string", Description: "Remote branch name. Defaults to current branch name."},
					"set_upstream": {Type: "boolean", Description: "Set remote as upstream (-u). Use this for first push of a new branch."},
					"force":        {Type: "boolean", Description: "Use --force-with-lease. Safer than hard force push, but still overwrites history."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Remote      string `json:"remote"`
				Branch      string `json:"branch"`
				SetUpstream bool   `json:"set_upstream"`
				Force       bool   `json:"force"`
			}
			json.Unmarshal(args, &a)
			if a.Remote == "" {
				a.Remote = "origin"
			}
			argv := []string{"push"}
			if a.SetUpstream {
				argv = append(argv, "-u")
			}
			if a.Force {
				argv = append(argv, "--force-with-lease")
			}
			argv = append(argv, a.Remote)
			if a.Branch != "" {
				argv = append(argv, a.Branch)
			}
			cmd := exec.CommandContext(ctx, "git", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return string(out), nil
		},
	}
}

func GitStashTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_stash",
			Description: "Stash working changes to switch contexts. Actions: 'push' (save changes, default), 'pop' (restore and remove latest), 'list' (show all stashes), 'drop' (delete latest). Optional message to label the stash.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"action":  {Type: "string", Description: "Action: 'push' (default), 'pop', 'list', or 'drop'."},
					"message": {Type: "string", Description: "Optional description for the stash (push only)."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Action  string `json:"action"`
				Message string `json:"message"`
			}
			json.Unmarshal(args, &a)
			if a.Action == "" {
				a.Action = "push"
			}
			var cmd *exec.Cmd
			switch a.Action {
			case "pop":
				cmd = exec.CommandContext(ctx, "git", "stash", "pop")
			case "list":
				cmd = exec.CommandContext(ctx, "git", "stash", "list")
			case "drop":
				cmd = exec.CommandContext(ctx, "git", "stash", "drop")
			default:
				argv := []string{"stash", "push"}
				if a.Message != "" {
					argv = append(argv, "-m", a.Message)
				}
				cmd = exec.CommandContext(ctx, "git", argv...)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			text := strings.TrimSpace(string(out))
			if text == "" {
				if a.Action == "list" {
					return "(no stashes)", nil
				}
				return "[ok]", nil
			}
			return text, nil
		},
	}
}

func GitMergeTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_merge",
			Description: "Merge a branch into the current branch. Use no_ff=true to always create a merge commit. Returns the merge result or conflict information.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"branch": {Type: "string", Description: "Branch to merge into current branch."},
					"no_ff":  {Type: "boolean", Description: "Create a merge commit even if fast-forward is possible. Default false."},
				},
				Required: []string{"branch"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Branch string `json:"branch"`
				NoFF   bool   `json:"no_ff"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Branch == "" {
				return "", fmt.Errorf("branch is required")
			}
			argv := []string{"merge"}
			if a.NoFF {
				argv = append(argv, "--no-ff")
			}
			argv = append(argv, a.Branch)
			cmd := exec.CommandContext(ctx, "git", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return strings.TrimSpace(string(out)), nil
		},
	}
}

func GitResetTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_reset",
			Description: "Unstage files or reset HEAD. With a path, unstages that file from the index. With a commit ref, resets HEAD to that commit. Modes: 'soft' (keep changes staged), 'mixed' (keep changes unstaged - default), 'hard' (DESTRUCTIVE: discard changes).",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"target": {Type: "string", Description: "File path to unstage, or commit ref to reset to (e.g. 'HEAD~1')."},
					"mode":   {Type: "string", Description: "Reset mode: 'soft', 'mixed' (default), or 'hard'. Only applies to commit resets."},
				},
				Required: []string{"target"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Target string `json:"target"`
				Mode   string `json:"mode"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Target == "" {
				return "", fmt.Errorf("target is required")
			}
			if a.Mode == "" {
				a.Mode = "mixed"
			}
			isFile := false
			if info, err := os.Stat(a.Target); err == nil && !info.IsDir() {
				isFile = true
			}
			var argv []string
			if isFile {
				argv = []string{"reset", "HEAD", "--", a.Target}
			} else {
				argv = []string{"reset"}
				switch a.Mode {
				case "hard":
					argv = append(argv, "--hard")
				case "soft":
					argv = append(argv, "--soft")
				default:
				}
				argv = append(argv, a.Target)
			}
			cmd := exec.CommandContext(ctx, "git", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			text := strings.TrimSpace(string(out))
			if text == "" {
				return "[ok]", nil
			}
			return text, nil
		},
	}
}

func GitRemoteTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "git_remote",
			Description: "Manage remote repositories. With no arguments, lists all remotes with their URLs (-v). Use action='add' to add a new remote, action='remove' to delete one, or action='show' to inspect a specific remote.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"action": {Type: "string", Description: "Action: 'list' (default), 'add', 'remove', or 'show'."},
					"name":   {Type: "string", Description: "Remote name (required for add/remove/show)."},
					"url":    {Type: "string", Description: "Remote URL (required for add)."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Action string `json:"action"`
				Name   string `json:"name"`
				URL    string `json:"url"`
			}
			json.Unmarshal(args, &a)
			if a.Action == "" {
				a.Action = "list"
			}
			var cmd *exec.Cmd
			switch a.Action {
			case "add":
				if a.Name == "" || a.URL == "" {
					return "", fmt.Errorf("name and url are required for add")
				}
				cmd = exec.CommandContext(ctx, "git", "remote", "add", a.Name, a.URL)
			case "remove":
				if a.Name == "" {
					return "", fmt.Errorf("name is required for remove")
				}
				cmd = exec.CommandContext(ctx, "git", "remote", "remove", a.Name)
			case "show":
				if a.Name == "" {
					return "", fmt.Errorf("name is required for show")
				}
				cmd = exec.CommandContext(ctx, "git", "remote", "show", a.Name)
			default:
				cmd = exec.CommandContext(ctx, "git", "remote", "-v")
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			text := strings.TrimSpace(string(out))
			if text == "" {
				return "[ok]", nil
			}
			return text, nil
		},
	}
}

func ProcessListTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "process_list",
			Description: "List running processes sorted by memory usage. Returns PID, CPU%, MEM%, and command. Filter by name to find specific processes. Use this to check if servers, builds, or background tasks are still running.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"name":  {Type: "string", Description: "Filter to processes whose command contains this substring (case-insensitive)."},
					"limit": {Type: "number", Description: "Maximum number of results. Default 20."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Name  string `json:"name"`
				Limit int    `json:"limit"`
			}
			json.Unmarshal(args, &a)
			if a.Limit <= 0 {
				a.Limit = 20
			}
			argv := []string{"aux", "--sort=-%mem"}
			cmd := exec.CommandContext(ctx, "ps", argv...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("ps failed: %w", err)
			}
			lines := strings.Split(string(out), "\n")
			header := lines[0]
			var matches []string
			for _, line := range lines[1:] {
				if line == "" {
					continue
				}
				if a.Name != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(a.Name)) {
					continue
				}
				matches = append(matches, line)
				if len(matches) >= a.Limit {
					break
				}
			}
			if len(matches) == 0 {
				if a.Name != "" {
					return fmt.Sprintf("no processes matching %q found", a.Name), nil
				}
				return header + "\n(no user processes)", nil
			}
			return header + "\n" + strings.Join(matches, "\n"), nil
		},
	}
}

func ProcessKillTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "process_kill",
			Description: "Kill a running process by PID or name. Default signal is 15 (SIGTERM - graceful). Use signal=9 for SIGKILL (force). Use by_name=true to kill all processes matching a name via pkill.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"target":  {Type: "string", Description: "PID or process name to kill."},
					"signal":  {Type: "number", Description: "Signal number. Default 15 (SIGTERM), use 9 for SIGKILL."},
					"by_name": {Type: "boolean", Description: "Treat target as a process name (uses pkill) instead of PID."},
				},
				Required: []string{"target"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Target string `json:"target"`
				Signal int    `json:"signal"`
				ByName bool   `json:"by_name"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Target == "" {
				return "", fmt.Errorf("target is required")
			}
			if a.Signal <= 0 {
				a.Signal = 15
			}
			var cmd *exec.Cmd
			if a.ByName {
				cmd = exec.CommandContext(ctx, "pkill", "-"+strconv.Itoa(a.Signal), a.Target)
			} else {
				cmd = exec.CommandContext(ctx, "kill", "-"+strconv.Itoa(a.Signal), a.Target)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), fmt.Errorf("kill failed: %w", err)
			}
			if a.ByName {
				return "sent signal " + strconv.Itoa(a.Signal) + " to processes matching " + a.Target, nil
			}
			return "sent signal " + strconv.Itoa(a.Signal) + " to PID " + a.Target, nil
		},
	}
}

func DiskUsageTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "disk_usage",
			Description: "Show disk usage for filesystems or directories. Omit path to see all mounted filesystems (df -h). Provide a path to see directory usage (du). Use max_depth to control how deep du recurses. Use this to check available space before large operations.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":      {Type: "string", Description: "Directory or mount point to check. If omitted, shows all real filesystems."},
					"max_depth": {Type: "number", Description: "For directory size analysis, how deep to summarize. Default 1 (immediate children)."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path     string `json:"path"`
				MaxDepth int    `json:"max_depth"`
			}
			json.Unmarshal(args, &a)
			if a.Path == "" {
				cmd := exec.CommandContext(ctx, "df", "-x", "tmpfs", "-x", "devtmpfs", "-x", "squashfs", "-h")
				out, err := cmd.CombinedOutput()
				if err != nil {
					return "", err
				}
				return string(out), nil
			}
			if a.MaxDepth <= 0 {
				a.MaxDepth = 1
			}
			cmd := exec.CommandContext(ctx, "du", "-h", "--max-depth="+strconv.Itoa(a.MaxDepth), a.Path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(out)), nil
		},
	}
}

func joinLines(s []string) string {
	if len(s) == 0 {
		return ""
	}
	out := s[0]
	for _, line := range s[1:] {
		out += "\n" + line
	}
	return out
}

func CodeIndexTool(embedder Embedder) Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "code_index",
			Description: "Build or rebuild a semantic embedding index for the current project. The index enables natural-language code search via semantic_search. You can force a rebuild with force_rebuild=true.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"model":         {Type: "string", Description: "Embedding model to use (default: nomic-embed-text)."},
					"force_rebuild": {Type: "boolean", Description: "Rebuild even if a cached index exists."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Model        string `json:"model"`
				ForceRebuild bool   `json:"force_rebuild"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if a.Model == "" {
				a.Model = "nomic-embed-text"
			}
			cwd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			if !a.ForceRebuild {
				if _, err := semantic.LoadIndex(cwd); err == nil {
					return "Semantic index already exists. Use force_rebuild=true to rebuild.", nil
				}
			}
			idx, err := semantic.BuildIndex(cwd, a.Model, func(inputs []string) ([][]float32, error) {
				return embedder.Embed(a.Model, inputs)
			})
			if err != nil {
				return "", fmt.Errorf("index build failed: %w", err)
			}
			if err := semantic.SaveIndex(idx); err != nil {
				return "", fmt.Errorf("save index failed: %w", err)
			}
			return fmt.Sprintf("Indexed %d chunks from project %s using model %s", len(idx.Chunks), cwd, a.Model), nil
		},
	}
}

func SemanticSearchTool(embedder Embedder) Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "semantic_search",
			Description: "Search the project with natural language using a pre-built semantic embedding index. Returns the top_k most relevant code snippets.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"query": {Type: "string", Description: "Natural language query."},
					"top_k": {Type: "number", Description: "Number of results to return. Default 5."},
					"model": {Type: "string", Description: "Embedding model used for the index (default: nomic-embed-text). Must match the model passed to code_index."},
				},
				Required: []string{"query"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Query string `json:"query"`
				TopK  int    `json:"top_k"`
				Model string `json:"model"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", err
			}
			if a.TopK <= 0 {
				a.TopK = 5
			}
			if a.Model == "" {
				a.Model = "nomic-embed-text"
			}
			cwd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			idx, err := semantic.LoadIndex(cwd)
			if err != nil {
				return "No semantic index found. Run code_index first.", nil
			}
			results, err := idx.Search(a.Query, func(q string) ([]float32, error) {
				embs, err := embedder.Embed(a.Model, []string{q})
				if err != nil {
					return nil, err
				}
				if len(embs) == 0 {
					return nil, fmt.Errorf("empty embedding response")
				}
				return embs[0], nil
			}, a.TopK)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for i, r := range results {
				fmt.Fprintf(&b, "%d. %s:%d-%d (score: %.3f)\n%s\n\n", i+1, r.Path, r.StartLine, r.EndLine, r.Score, r.Text)
			}
			return strings.TrimSpace(b.String()), nil
		},
	}
}
