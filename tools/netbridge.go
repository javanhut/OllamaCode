package tools

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// Linux half of the proxy design. bwrap --unshare-net gives the sandbox a
// private network namespace whose loopback is NOT the host's, so the host-side
// proxy on 127.0.0.1:<port> is unreachable. The standard fix, used here:
//
//   host proxy --listens--> unix socket in a private 0700 temp dir
//   bwrap --bind <dir> <dir>              (the socket is path-addressed, so it
//                                          crosses the namespace boundary)
//   in-sandbox forwarder: 127.0.0.1:netBridgePort (inside the netns) <-> socket
//
// The forwarder is the ocode binary itself, re-executed as the first process in
// the sandbox with OCODE_NETBRIDGE=1 (set via bwrap --setenv). The tools
// package's init() notices the variable before main runs, starts the relay
// listener, then runs the real command (`sh -c ...`, passed as its argv) as a
// child and exits with its status. This needs no socat on the host, no main.go
// wiring, and no extra files; the cost is that the binary must be readable
// inside the sandbox, which `--ro-bind / /` already guarantees.

const (
	netBridgePort    = "3128"
	netBridgeEnvFlag = "OCODE_NETBRIDGE"
	netBridgeEnvSock = "OCODE_NETBRIDGE_SOCK"
)

func init() {
	if os.Getenv(netBridgeEnvFlag) == "1" {
		os.Exit(runNetBridge(os.Getenv(netBridgeEnvSock), "127.0.0.1:"+netBridgePort, os.Args[1:]))
	}
}

// runNetBridge listens on listenAddr, relays each connection to the Unix
// socket, runs argv, and returns its exit code.
func runNetBridge(sock, listenAddr string, argv []string) int {
	if sock == "" || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "ocode netbridge: missing socket or command")
		return 125
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ocode netbridge:", err)
		return 125
	}
	go serveBridge(ln, func() (net.Conn, error) { return net.Dial("unix", sock) })

	cmd := exec.Command(argv[0], argv[1:]...)
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, netBridgeEnvFlag+"=") && !strings.HasPrefix(e, netBridgeEnvSock+"=") {
			env = append(env, e)
		}
	}
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "ocode netbridge:", err)
		return 127
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	signal.Stop(sigs)
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		return 126
	}
	return 0
}

// serveBridge accepts on ln and relays each connection to a fresh dial().
// Pure (no namespace assumptions), so it is testable on any OS.
func serveBridge(ln net.Listener, dial func() (net.Conn, error)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			up, err := dial()
			if err != nil {
				return
			}
			defer up.Close()
			relayIdle(c, up, netProxyIdleTimeout)
		}()
	}
}
