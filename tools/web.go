package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"
)

func newWebClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
	}
}

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
			if slices.Contains(classes, class) {
				return true
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

var webUA = "Mozilla/5.0 (compatible; OllamaCode/1.0; +https://github.com/javanhut/ollama_code)"
