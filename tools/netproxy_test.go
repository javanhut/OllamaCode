package tools

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMatchAllowlist(t *testing.T) {
	entries := parseAllowlist([]string{"PyPI.org", "*.githubusercontent.com", "internal.test:8443", "10.1.2.3", " https://crates.io/ ", "*", ""})
	if len(entries) != 5 {
		t.Fatalf("expected 5 valid entries (bare * and empty dropped), got %v", entries)
	}
	cases := []struct {
		host, port string
		want       bool
	}{
		{"pypi.org", "443", true},
		{"PYPI.ORG", "80", true},
		{"pypi.org.", "443", true},
		{"pypi.org", "8080", false},
		{"evilpypi.org", "443", false},
		{"x.pypi.org", "443", false}, // exact entries do not match subdomains
		{"raw.githubusercontent.com", "443", true},
		{"a.b.githubusercontent.com", "443", true},
		{"githubusercontent.com", "443", false}, // wildcard excludes the apex
		{"evilgithubusercontent.com", "443", false},
		{"internal.test", "8443", true},
		{"internal.test", "443", false}, // an explicit port replaces the default 80/443
		{"10.1.2.3", "443", true},       // IP literal only when listed exactly
		{"10.1.2.4", "443", false},
		{"93.184.216.34", "443", false},
		{"crates.io", "443", true},
	}
	for _, c := range cases {
		if got := matchAllowlist(entries, c.host, c.port); got != c.want {
			t.Errorf("match(%q,%q)=%v want %v", c.host, c.port, got, c.want)
		}
	}
	// A wildcard must never match an IP literal.
	if matchAllowlist(parseAllowlist([]string{"*.0.0.1"}), "127.0.0.1", "443") {
		t.Error("wildcard matched an IP literal")
	}
}

func TestBlockedAddr(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "10.0.0.1", "172.16.5.5", "192.168.1.1", "169.254.169.254",
		"fe80::1", "fd00::1", "fc00::1", "0.0.0.0", "100.100.100.200", "::ffff:127.0.0.1", "::ffff:10.0.0.1"} {
		if _, bad := blockedAddr(netip.MustParseAddr(s)); !bad {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range []string{"93.184.216.34", "140.82.112.3", "2606:4700::1111"} {
		if why, bad := blockedAddr(netip.MustParseAddr(s)); bad {
			t.Errorf("%s must be allowed, blocked as %s", s, why)
		}
	}
}

// proxyFixture starts the shared proxy with an allowlist and test hooks that
// resolve every name to resolveTo and dial it at the given upstream address
// (a loopback httptest server). It returns the proxy and the IPs it dialed.
func proxyFixture(t *testing.T, upstream, resolveTo string, allow ...string) (*netProxy, *[]netip.Addr) {
	t.Helper()
	_, port, _ := net.SplitHostPort(upstream)
	oldLookup, oldDial := netLookup, netDialAddr
	var dialed []netip.Addr
	netLookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(resolveTo)}, nil
	}
	netDialAddr = func(ip netip.Addr, _ string) string {
		dialed = append(dialed, ip)
		return net.JoinHostPort("127.0.0.1", port)
	}
	netAllowLoopback.Store(resolveTo == "127.0.0.1")
	SetShellNetworkAllowlist(allow)
	p, err := ensureNetProxy()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		netLookup, netDialAddr = oldLookup, oldDial
		netAllowLoopback.Store(false)
		SetShellNetworkAllowlist(nil)
		CloseNetProxy()
	})
	return p, &dialed
}

func proxyClient(p *netProxy) *http.Client {
	u, _ := url.Parse(p.url())
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(u),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
}

func TestProxyConnectAllowedAndDenied(t *testing.T) {
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure-ok") }))
	defer up.Close()
	p, _ := proxyFixture(t, up.Listener.Addr().String(), "127.0.0.1", "allowed.test")
	c := proxyClient(p)

	resp, err := c.Get("https://Allowed.Test/x")
	if err != nil {
		t.Fatalf("allowed CONNECT failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "secure-ok" {
		t.Errorf("tunnel body = %q", body)
	}

	if _, err := c.Get("https://denied.test/"); err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("denied CONNECT must be refused with 403, got %v", err)
	}
	// Raw CONNECT to read the 403 body.
	conn, _ := net.Dial("tcp", p.tcp.Addr().String())
	defer conn.Close()
	fmt.Fprint(conn, "CONNECT denied.test:443 HTTP/1.1\r\nHost: denied.test:443\r\n\r\n")
	cr, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(cr.Body)
	if cr.StatusCode != 403 || !strings.Contains(string(raw), "shell_network_allow") {
		t.Errorf("403 body unclear: %d %q", cr.StatusCode, raw)
	}
	// Wrong port for a default-port entry.
	if _, err := c.Get("https://allowed.test:8443/"); err == nil {
		t.Error("non-443/80 port must be denied")
	}

	denied := NetProxyDeniedHosts(10)
	if len(denied) == 0 || !contains(denied, "denied.test") || !contains(denied, "allowed.test:8443") {
		t.Errorf("denied hosts = %v", denied)
	}
}

func TestProxyPlainHTTPForward(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Host", r.Host)
		w.Header().Set("Connection", "close")
		io.WriteString(w, "plain-ok "+r.URL.Path)
	}))
	defer up.Close()
	p, _ := proxyFixture(t, up.Listener.Addr().String(), "127.0.0.1", "allowed.test")
	c := proxyClient(p)

	resp, err := c.Get("http://allowed.test/pkg")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "plain-ok /pkg" || resp.Header.Get("X-Seen-Host") != "allowed.test" {
		t.Errorf("forward: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	resp, err = c.Get("http://denied.test/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 403 || !strings.Contains(string(b), "denied by shell_network_allow") {
		t.Errorf("denied forward: %d %q", resp.StatusCode, b)
	}
	// Direct (non-proxy) requests are not served.
	r2, err := http.Get(p.url() + "/direct")
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusBadRequest {
		t.Errorf("origin-form request status = %d, want 400", r2.StatusCode)
	}
}

func TestProxyRefusesPrivateResolution(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "SECRET") }))
	defer up.Close()
	for _, ip := range []string{"10.0.0.5", "192.168.1.9", "169.254.169.254", "fd00::1", "::ffff:127.0.0.1", "127.0.0.1"} {
		p, dialed := proxyFixture(t, up.Listener.Addr().String(), ip, "rebind.test")
		netAllowLoopback.Store(false) // production behaviour: loopback is blocked too
		resp, err := proxyClient(p).Get("http://rebind.test/")
		if err != nil {
			t.Fatalf("%s: %v", ip, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 403 || strings.Contains(string(b), "SECRET") || len(*dialed) != 0 {
			t.Errorf("%s: want 403 and no dial, got %d %q dialed=%v", ip, resp.StatusCode, b, *dialed)
		}
		if _, err := proxyClient(p).Get("https://rebind.test/"); err == nil || !strings.Contains(err.Error(), "Forbidden") {
			t.Errorf("%s: CONNECT must be 403, got %v", ip, err)
		}
		evs := NetProxyLog()
		if last := evs[len(evs)-1]; last.Allowed || !strings.Contains(last.Reason, "address") {
			t.Errorf("%s: log event = %+v", ip, last)
		}
		CloseNetProxy()
	}
}

func TestProxyDialsVettedIPNotName(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	p, dialed := proxyFixture(t, up.Listener.Addr().String(), "93.184.216.34", "pub.test")
	resp, err := proxyClient(p).Get("http://pub.test/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(*dialed) != 1 || (*dialed)[0].String() != "93.184.216.34" {
		t.Errorf("proxy must dial the resolved IP, dialed %v", *dialed)
	}
}

func TestProxyIPLiteral(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	p, _ := proxyFixture(t, up.Listener.Addr().String(), "127.0.0.1", "allowed.test")
	resp, err := proxyClient(p).Get("http://93.184.216.34/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("unlisted IP literal must be denied, got %d", resp.StatusCode)
	}
}

func TestEventRingBounded(t *testing.T) {
	var r eventRing
	for i := range netProxyLogSize + 50 {
		r.add(NetProxyEvent{Host: fmt.Sprintf("h%d", i)})
	}
	got := r.snapshot()
	if len(got) != netProxyLogSize || got[0].Host != "h50" || got[len(got)-1].Host != fmt.Sprintf("h%d", netProxyLogSize+49) {
		t.Errorf("ring wrong: len=%d first=%s last=%s", len(got), got[0].Host, got[len(got)-1].Host)
	}
}

func TestProxyEnvironmentOverrides(t *testing.T) {
	env := overrideEnv([]string{"PATH=/bin", "HTTP_PROXY=http://corp:1", "https_proxy=http://corp:1"}, proxyEnvironment("http://127.0.0.1:9"))
	count := map[string]int{}
	vals := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		count[k]++
		vals[k] = v
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if count[k] != 1 || vals[k] != "http://127.0.0.1:9" {
			t.Errorf("%s = %q (x%d)", k, vals[k], count[k])
		}
	}
	if vals["NO_PROXY"] != "localhost,127.0.0.1,::1" || vals["no_proxy"] != vals["NO_PROXY"] || vals["PATH"] != "/bin" {
		t.Errorf("env = %v", env)
	}
}

func withSandboxKind(t *testing.T, kind string) {
	t.Helper()
	old := sandboxKind.Load()
	sandboxKind.Store(&kind)
	t.Cleanup(func() { sandboxKind.Store(old) })
}

func TestShellLaunchSeatbeltProxy(t *testing.T) {
	withSandboxKind(t, "sandbox-exec")
	SetShellNetworkAllowed(false)
	SetShellNetworkAllowlist([]string{"pypi.org"})
	t.Cleanup(func() { SetShellNetworkAllowlist(nil); CloseNetProxy() })
	argv, env := shellLaunch("pip install x")
	profile := argv[2]
	// Profile stays loopback-only: the proxy lives on 127.0.0.1.
	if !strings.Contains(profile, "(deny network*)") || !strings.Contains(profile, `(allow network-outbound (remote ip "localhost:*"))`) ||
		strings.Contains(profile, "(allow network*") || strings.Contains(profile, `"*:443"`) {
		t.Errorf("profile must stay loopback-only:\n%s", profile)
	}
	p, _ := ensureNetProxy()
	if !contains(env, "HTTPS_PROXY="+p.url()) || !contains(env, "NO_PROXY=localhost,127.0.0.1,::1") || !contains(env, "all_proxy="+p.url()) {
		t.Errorf("env = %v", env)
	}
	// No allowlist: no proxy env, same profile.
	SetShellNetworkAllowlist(nil)
	if _, env := shellLaunch("true"); len(env) != 0 {
		t.Errorf("empty allowlist must not set proxy env: %v", env)
	}
	// Full network wins over the allowlist.
	SetShellNetworkAllowlist([]string{"pypi.org"})
	SetShellNetworkAllowed(true)
	defer SetShellNetworkAllowed(false)
	if _, env := shellLaunch("true"); len(env) != 0 {
		t.Errorf("shell_network=true must bypass the proxy: %v", env)
	}
}

func TestBwrapBridgeArgvShape(t *testing.T) {
	argv := bwrapBridgeArgv("echo hi", []string{"/ws"}, []string{"/ws/.git/hooks"}, nil, "/tmp/ocode-netproxy-1", "/tmp/ocode-netproxy-1/proxy.sock", "/usr/bin/ocode")
	j := strings.Join(argv, " ")
	for _, want := range []string{"--unshare-net", "--bind /ws /ws", "--bind /tmp/ocode-netproxy-1 /tmp/ocode-netproxy-1",
		"--setenv OCODE_NETBRIDGE 1", "--setenv OCODE_NETBRIDGE_SOCK /tmp/ocode-netproxy-1/proxy.sock"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
	if !strings.HasSuffix(j, "-- /usr/bin/ocode sh -c echo hi") {
		t.Errorf("must end with the forwarder wrapping the command: %s", j)
	}
	if strings.Count(j, " -- ") != 1 {
		t.Errorf("exactly one -- separator expected: %s", j)
	}
}

func TestShellLaunchBwrapProxy(t *testing.T) {
	withSandboxKind(t, "bwrap")
	SetShellNetworkAllowed(false)
	SetShellNetworkAllowlist([]string{"pypi.org"})
	t.Cleanup(func() { SetShellNetworkAllowlist(nil); CloseNetProxy() })
	argv, env := shellLaunch("true")
	j := strings.Join(argv, " ")
	if !strings.Contains(j, "--unshare-net") || !strings.Contains(j, "--setenv OCODE_NETBRIDGE 1") {
		t.Errorf("bwrap proxy argv: %s", j)
	}
	if !contains(env, "HTTPS_PROXY=http://127.0.0.1:"+netBridgePort) {
		t.Errorf("env must point at the in-sandbox bridge port: %v", env)
	}
	p, _ := ensureNetProxy()
	sock, _ := p.unixSocket()
	if fi, err := os.Stat(filepath.Dir(sock)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("socket dir must be 0700: %v %v", fi, err)
	}
	CloseNetProxy()
	if _, err := os.Stat(sock); err == nil {
		t.Error("CloseNetProxy must remove the socket dir")
	}
}

func TestNetworkHintWithAllowlist(t *testing.T) {
	withSandboxKind(t, "bwrap")
	SetShellNetworkAllowed(false)
	SetShellNetworkAllowlist([]string{"pypi.org"})
	t.Cleanup(func() { SetShellNetworkAllowlist(nil) })
	netEvents.add(NetProxyEvent{Host: "evil.example", Port: "443"})
	out := withNetworkHint("curl: (56) Received HTTP code 403 from proxy after CONNECT")
	if !strings.Contains(out, "evil.example") || !strings.Contains(out, "shell_network_allow") {
		t.Errorf("hint must name the denied host and the config key: %s", out)
	}
	if strings.Contains(withNetworkHint("all good"), "note:") {
		t.Error("no hint without a failure marker")
	}
	SetShellNetworkAllowlist(nil)
	if out := withNetworkHint("could not resolve host"); !strings.Contains(out, `"shell_network": true`) {
		t.Errorf("without allowlist the original hint stays: %s", out)
	}
}

// TestServeBridgeRelaysTCPToUnix is the Linux forwarder's logic, OS-agnostic:
// TCP listener <-> Unix socket echo server.
func TestServeBridgeRelaysTCPToUnix(t *testing.T) {
	dir, err := os.MkdirTemp("", "nb")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	uln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer uln.Close()
	go func() {
		for {
			c, err := uln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	tln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tln.Close()
	go serveBridge(tln, func() (net.Conn, error) { return net.Dial("unix", sock) })

	for i := range 3 { // several concurrent-ish connections
		c, err := net.Dial("tcp", tln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		msg := fmt.Sprintf("hello-%d", i)
		fmt.Fprint(c, msg)
		c.(*net.TCPConn).CloseWrite()
		got, _ := io.ReadAll(c)
		c.Close()
		if string(got) != msg {
			t.Errorf("echo = %q want %q", got, msg)
		}
	}
}

func TestRunNetBridgeExitCodeAndEnv(t *testing.T) {
	t.Setenv(netBridgeEnvFlag, "1")
	t.Setenv(netBridgeEnvSock, "/nonexistent")
	if code := runNetBridge("/nonexistent", "127.0.0.1:0", []string{"sh", "-c", "exit 3"}); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	// The child must not see the bridge variables.
	if code := runNetBridge("/nonexistent", "127.0.0.1:0", []string{"sh", "-c", `test -z "$OCODE_NETBRIDGE$OCODE_NETBRIDGE_SOCK"`}); code != 0 {
		t.Errorf("bridge env leaked into child (code %d)", code)
	}
}

func TestRelayIdleTimeout(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	done := make(chan struct{})
	go func() { relayIdle(a1, b1, 100*time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("idle tunnel was not torn down")
	}
}

// TestSandboxProxyEndToEnd runs the real macOS sandbox: via-proxy requests to an
// allowed name work (loopback test upstream), a denied name gets 403, and a
// direct connection that bypasses the proxy to a non-loopback IP fails.
func TestSandboxProxyEndToEnd(t *testing.T) {
	if detectShellSandbox() != "sandbox-exec" {
		t.Skip("real-sandbox proxy test runs on macOS only")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	root := t.TempDir()
	pinJail(t, root)
	SetShellNetworkAllowed(false)
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	proxyFixture(t, up.Listener.Addr().String(), "127.0.0.1", "allowed.test")

	run := func(command string) (string, error) {
		cmd := newShellCommand(command)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run(`curl -sS -k --max-time 10 -o /dev/null -w '%{http_code}' https://allowed.test/`); err != nil || out != "200" {
		t.Errorf("allowed via proxy: %q %v", out, err)
	}
	// curl cannot resolve names itself in the sandbox: success proves the proxy resolved it.
	if out, _ := run(`curl -sS -k --max-time 10 -o /dev/null -w '%{http_connect}' https://denied.test/ 2>/dev/null`); out != "403" {
		t.Errorf("denied via proxy: CONNECT status %q, want 403", out)
	}
	if out, _ := run(`curl -sS --max-time 10 -o /dev/null -w '%{http_code}' http://denied.test/`); out != "403" {
		t.Errorf("denied plain http: %q, want 403", out)
	}
	if out, err := run(`curl -sS --noproxy '*' --max-time 5 -o /dev/null -w '%{http_code}' https://1.1.1.1/`); err == nil {
		t.Errorf("direct egress bypassing the proxy must fail, got %q", out)
	}
	if out, err := run(`curl -sS --noproxy '*' --max-time 5 -o /dev/null http://93.184.216.34/`); err == nil {
		t.Errorf("direct plain-http egress must fail, got %q", out)
	}
}
