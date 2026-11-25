package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Domain-allowlist egress proxy for the shell sandbox.
//
// shell_network=false denies everything but loopback; shell_network=true opens
// everything. The middle ground (shell_network_allow) runs an HTTP(S) proxy in
// THIS process, outside the sandbox. The sandboxed command can only reach the
// proxy (loopback on macOS; a Unix socket bridged to an in-sandbox loopback
// listener on Linux, see netbridge.go), and HTTP_PROXY & co. point tools at it.
//
// The proxy never inspects TLS: CONNECT tunnels are opaque byte relays gated on
// the target host name alone. Name resolution happens HERE, and every resolved
// address is vetted (no loopback, RFC1918, link-local/metadata, unique-local,
// CGNAT) before the proxy dials that vetted IP — never the name — so neither
// DNS rebinding nor a re-resolution race can steer an allowed name at an
// internal service.

// DefaultPackageRegistryDomains is a starting allowlist a UI can offer for
// "let builds fetch dependencies". It is NOT enabled by default. Note that
// every entry is also a place data can be sent (see docs/safety.md).
var DefaultPackageRegistryDomains = []string{
	"proxy.golang.org", "sum.golang.org", "storage.googleapis.com",
	"registry.npmjs.org", "registry.yarnpkg.com",
	"pypi.org", "files.pythonhosted.org",
	"crates.io", "static.crates.io", "index.crates.io",
	"github.com", "codeload.github.com", "*.githubusercontent.com",
	"rubygems.org", "repo.maven.apache.org", "repo1.maven.org",
}

const (
	netProxyMaxConns    = 256
	netProxyIdleTimeout = 5 * time.Minute
	netProxyDialTimeout = 15 * time.Second
	netProxyLogSize     = 200
)

// allowEntry is one parsed allowlist item.
type allowEntry struct {
	host     string // lowercase; for wildcards, the suffix without "*."
	wildcard bool
	port     string // "" = 80 or 443 only
}

func (e allowEntry) String() string {
	s := e.host
	if e.wildcard {
		s = "*." + s
	}
	if e.port != "" {
		s += ":" + e.port
	}
	return s
}

func parseAllowEntry(raw string) (allowEntry, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.TrimRight(s, "/")
	var e allowEntry
	if strings.HasPrefix(s, "[") {
		if h, p, err := net.SplitHostPort(s); err == nil {
			s, e.port = "["+h+"]", p
		}
	} else if strings.Count(s, ":") == 1 {
		s, e.port, _ = strings.Cut(s, ":")
	}
	s = strings.TrimSuffix(strings.Trim(s, "[]"), ".")
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		e.wildcard, s = true, rest
	}
	if s == "" || s == "*" || strings.ContainsAny(s, "*/ ") {
		return allowEntry{}, false
	}
	if e.port != "" {
		if n, err := strconv.Atoi(e.port); err != nil || n < 1 || n > 65535 {
			return allowEntry{}, false
		}
	}
	e.host = s
	return e, true
}

func parseAllowlist(domains []string) []allowEntry {
	var out []allowEntry
	seen := map[string]bool{}
	for _, d := range domains {
		if e, ok := parseAllowEntry(d); ok && !seen[e.String()] {
			seen[e.String()] = true
			out = append(out, e)
		}
	}
	return out
}

// matchAllowlist reports whether host:port may be reached. IP literals match
// only an exact, non-wildcard entry; wildcards match subdomains, not the apex.
func matchAllowlist(entries []allowEntry, host, port string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	_, isIP := parseIPLiteral(host)
	for _, e := range entries {
		if e.port == "" {
			if port != "443" && port != "80" {
				continue
			}
		} else if e.port != port {
			continue
		}
		switch {
		case e.wildcard:
			if !isIP && strings.HasSuffix(host, "."+e.host) {
				return true
			}
		case e.host == host:
			return true
		}
	}
	return false
}

func parseIPLiteral(host string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// blockedAddr reports whether an address is off-limits to the proxy even for
// an allowlisted name: anything that is not a public unicast address.
func blockedAddr(a netip.Addr) (string, bool) {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return "loopback", true
	case a.IsPrivate():
		return "private", true
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return "link-local", true
	case a.IsUnspecified():
		return "unspecified", true
	case a.IsMulticast():
		return "multicast", true
	case cgnat.Contains(a):
		return "carrier-grade NAT", true
	}
	return "", false
}

// Test hooks. Production values are the defaults.
var (
	netLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	netDialAddr      = func(ip netip.Addr, port string) string { return net.JoinHostPort(ip.String(), port) }
	netAllowLoopback atomic.Bool // tests only: loopback upstreams are not blocked
)

var errBlockedAddr = errors.New("blocked address")

// dialVetted resolves host, refuses if ANY answer is non-public, and dials the
// vetted IP. An IP-literal host reaches here only because the allowlist named
// it explicitly, so it skips the guard.
func dialVetted(ctx context.Context, host, port string) (net.Conn, error) {
	var addrs []netip.Addr
	if ip, ok := parseIPLiteral(host); ok {
		addrs = []netip.Addr{ip}
	} else {
		res, err := netLookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		for _, a := range res {
			if why, bad := blockedAddr(a); bad && !(netAllowLoopback.Load() && a.Unmap().IsLoopback()) {
				return nil, fmt.Errorf("%w: %s resolves to %s address %s", errBlockedAddr, host, why, a.Unmap())
			}
		}
		addrs = res
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	d := net.Dialer{Timeout: netProxyDialTimeout}
	var lastErr error
	for _, a := range addrs {
		c, err := d.DialContext(ctx, "tcp", netDialAddr(a.Unmap(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// NetProxyEvent is one allow/deny decision.
type NetProxyEvent struct {
	Time    time.Time
	Method  string // CONNECT, GET, ...
	Host    string
	Port    string
	Allowed bool
	Reason  string // why it was denied or failed; empty when allowed
}

type eventRing struct {
	mu     sync.Mutex
	events []NetProxyEvent // oldest first, capped at netProxyLogSize
}

func (r *eventRing) add(e NetProxyEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == netProxyLogSize {
		copy(r.events, r.events[1:])
		r.events = r.events[:netProxyLogSize-1]
	}
	r.events = append(r.events, e)
}

func (r *eventRing) snapshot() []NetProxyEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]NetProxyEvent(nil), r.events...)
}

var netEvents eventRing

// NetProxyLog returns the recent proxy decisions, oldest first (at most 200).
func NetProxyLog() []NetProxyEvent { return netEvents.snapshot() }

// NetProxyDeniedHosts returns the distinct hosts (host:port) denied recently,
// most recent first, at most limit.
func NetProxyDeniedHosts(limit int) []string {
	evs := netEvents.snapshot()
	var out []string
	seen := map[string]bool{}
	for i := len(evs) - 1; i >= 0 && len(out) < limit; i-- {
		e := evs[i]
		if e.Allowed {
			continue
		}
		hp := e.Host
		if e.Port != "" && e.Port != "443" && e.Port != "80" {
			hp += ":" + e.Port
		}
		if !seen[hp] {
			seen[hp] = true
			out = append(out, hp)
		}
	}
	return out
}

// Allowlist state. Read per request, so config changes apply to the running
// proxy without a restart.
var netAllow atomic.Pointer[[]allowEntry]

// SetShellNetworkAllowlist is the config switch shell_network_allow (default
// empty = no proxy). Entries are exact hosts ("pypi.org"), wildcard suffixes
// ("*.githubusercontent.com"), optionally with ":port". It only has effect
// while the sandbox is on and shell_network is false.
func SetShellNetworkAllowlist(domains []string) {
	e := parseAllowlist(domains)
	netAllow.Store(&e)
}

// ShellNetworkAllowlist returns the normalized active entries.
func ShellNetworkAllowlist() []string {
	p := netAllow.Load()
	if p == nil {
		return nil
	}
	out := make([]string, len(*p))
	for i, e := range *p {
		out[i] = e.String()
	}
	return out
}

func currentAllowlist() []allowEntry {
	if p := netAllow.Load(); p != nil {
		return *p
	}
	return nil
}

// netProxy is the shared server.
type netProxy struct {
	srv       *http.Server
	tcp       net.Listener
	transport *http.Transport
	sem       chan struct{}

	unixMu   sync.Mutex
	unixDir  string
	unixPath string
}

var (
	netProxyMu   sync.Mutex
	netProxyInst *netProxy
)

// ensureNetProxy starts the proxy on first use and returns it.
func ensureNetProxy() (*netProxy, error) {
	netProxyMu.Lock()
	defer netProxyMu.Unlock()
	if netProxyInst != nil {
		return netProxyInst, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &netProxy{tcp: ln, sem: make(chan struct{}, netProxyMaxConns)}
	p.transport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			h, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return dialVetted(ctx, h, port)
		},
		DisableCompression:    true,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go p.srv.Serve(ln)
	netProxyInst = p
	return p, nil
}

func (p *netProxy) url() string { return "http://" + p.tcp.Addr().String() }

// unixSocket lazily listens on a Unix socket in a private (0700) temp dir, for
// the Linux bridge; returns the socket path.
func (p *netProxy) unixSocket() (string, error) {
	p.unixMu.Lock()
	defer p.unixMu.Unlock()
	if p.unixPath != "" {
		return p.unixPath, nil
	}
	dir, err := os.MkdirTemp("", "ocode-netproxy-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "proxy.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	p.unixDir, p.unixPath = dir, path
	go p.srv.Serve(ln)
	return path, nil
}

// CloseNetProxy stops the proxy and removes its socket dir. Safe to call when
// it never started. Wire as a defer in main alongside the other cleanups.
func CloseNetProxy() {
	netProxyMu.Lock()
	p := netProxyInst
	netProxyInst = nil
	netProxyMu.Unlock()
	if p == nil {
		return
	}
	p.srv.Close()
	p.transport.CloseIdleConnections()
	p.unixMu.Lock()
	if p.unixDir != "" {
		os.RemoveAll(p.unixDir)
	}
	p.unixMu.Unlock()
}

func (p *netProxy) deny(w http.ResponseWriter, method, host, port, reason string, code int) {
	netEvents.add(NetProxyEvent{Time: time.Now(), Method: method, Host: host, Port: port, Reason: reason})
	w.Header().Set("X-Ocode-Proxy-Denied", "1")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, "ocode sandbox: network access to %s:%s denied by shell_network_allow (%s). Ask the user to add the domain to \"shell_network_allow\" in config.json.\n", host, port, reason)
}

func (p *netProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var host, port string
	if r.Method == http.MethodConnect {
		h, pt, err := net.SplitHostPort(r.Host)
		if err != nil {
			h, pt = strings.Trim(r.Host, "[]"), "443"
		}
		host, port = h, pt
	} else {
		if !r.URL.IsAbs() || r.URL.Scheme != "http" {
			http.Error(w, "ocode sandbox proxy: only absolute http:// requests and CONNECT are supported", http.StatusBadRequest)
			return
		}
		host, port = r.URL.Hostname(), r.URL.Port()
		if port == "" {
			port = "80"
		}
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !matchAllowlist(currentAllowlist(), host, port) {
		p.deny(w, r.Method, host, port, "host not in allowlist", http.StatusForbidden)
		return
	}
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	default:
		p.deny(w, r.Method, host, port, "too many concurrent connections", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodConnect {
		p.tunnel(w, r, host, port)
	} else {
		p.forward(w, r, host, port)
	}
}

func (p *netProxy) dialFailed(w http.ResponseWriter, r *http.Request, host, port string, err error) {
	if errors.Is(err, errBlockedAddr) {
		p.deny(w, r.Method, host, port, err.Error(), http.StatusForbidden)
		return
	}
	netEvents.add(NetProxyEvent{Time: time.Now(), Method: r.Method, Host: host, Port: port, Allowed: true, Reason: "upstream: " + err.Error()})
	http.Error(w, "ocode sandbox proxy: upstream connect failed: "+err.Error(), http.StatusBadGateway)
}

func (p *netProxy) tunnel(w http.ResponseWriter, r *http.Request, host, port string) {
	up, err := dialVetted(r.Context(), host, port)
	if err != nil {
		p.dialFailed(w, r, host, port, err)
		return
	}
	defer up.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	client.SetDeadline(time.Time{})
	netEvents.add(NetProxyEvent{Time: time.Now(), Method: r.Method, Host: host, Port: port, Allowed: true})
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if n := brw.Reader.Buffered(); n > 0 {
		b, _ := brw.Reader.Peek(n)
		up.Write(b)
	}
	relay(client, up)
}

// idleConn refreshes a deadline on every read and write, so a tunnel dies
// after netProxyIdleTimeout of silence in both directions' own sense.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c idleConn) Read(b []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

func (c idleConn) Write(b []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(b)
}

func (c idleConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// relay copies both ways until both directions finish or idle out.
func relay(a, b net.Conn) { relayIdle(a, b, netProxyIdleTimeout) }

func relayIdle(a, b net.Conn, idle time.Duration) {
	ia, ib := idleConn{a, idle}, idleConn{b, idle}
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src idleConn) {
		defer wg.Done()
		io.Copy(dst, src)
		dst.CloseWrite()
	}
	go cp(ib, ia)
	go cp(ia, ib)
	wg.Wait()
}

var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func stripHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(tok))
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func (p *netProxy) forward(w http.ResponseWriter, r *http.Request, host, port string) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Close = false
	stripHop(out.Header)
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		p.dialFailed(w, r, host, port, err)
		return
	}
	defer resp.Body.Close()
	netEvents.add(NetProxyEvent{Time: time.Now(), Method: r.Method, Host: host, Port: port, Allowed: true})
	stripHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// proxyEnvironment is the environment that points tools at proxyURL. NO_PROXY
// keeps loopback direct: tests that start a local server must not route
// through the allowlist.
func proxyEnvironment(proxyURL string) []string {
	var env []string
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
		env = append(env, k+"="+proxyURL)
	}
	return append(env, "NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1")
}

// overrideEnv returns env with the extra entries replacing same-named ones.
func overrideEnv(env, extra []string) []string {
	if len(extra) == 0 {
		return env
	}
	drop := map[string]bool{}
	for _, e := range extra {
		k, _, _ := strings.Cut(e, "=")
		drop[k] = true
	}
	out := make([]string, 0, len(env)+len(extra))
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); !drop[k] {
			out = append(out, e)
		}
	}
	return append(out, extra...)
}

// netProxyActive reports whether shell commands should be routed through the
// proxy: sandboxed, network not fully open, allowlist non-empty.
func netProxyActive() bool {
	return shellSandboxEnabled() && detectShellSandbox() != "" && !shellNetworkAllowed() && len(currentAllowlist()) > 0
}

// deniedHostsNote renders the recent denied hosts for hints.
func deniedHostsNote() string {
	hosts := NetProxyDeniedHosts(5)
	sort.Strings(hosts)
	return strings.Join(hosts, ", ")
}
