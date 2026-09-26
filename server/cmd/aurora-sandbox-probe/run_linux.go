//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// defaultDial bounds every probe network attempt so a blackholed route cannot
// hang the acceptance test.
const defaultDial = 3 * time.Second

// proxyTimeout bounds one proxy probe request.
const proxyTimeout = 15 * time.Second

// result is one probe's machine-readable outcome. Allowed reports whether the
// sandbox permitted the operation; the acceptance test requires false for every
// boundary probe and true for its positive controls.
type result struct {
	Probe   string `json:"probe"`
	Allowed bool   `json:"allowed"`
	Detail  string `json:"detail,omitempty"`
}

// run dispatches one probe subcommand. Every recognized probe exits 0 after
// printing its JSON result; only an unknown probe or an encode failure is an
// internal error.
func run(args []string) int {
	if len(args) == 0 {
		return emit(result{Probe: "usage", Allowed: false, Detail: "no probe named"})
	}
	name, rest := args[0], args[1:]
	switch name {
	case "serve":
		// The sandbox container's long-lived command. A plain select{} would
		// trip the Go runtime deadlock detector, so block on a timer instead.
		for {
			time.Sleep(24 * time.Hour)
		}
	case "exit-now":
		return 0
	case "listen":
		return probeListen(rest)
	case "fs-write-root":
		return probeFSWrite("/aurora-probe-root-write", name)
	case "fs-write-workspace":
		return probeFSWrite("/workspace/aurora-probe-write", name)
	case "privilege-escalate":
		return probePrivilegeEscalation()
	case "mount":
		return probeMount()
	case "unshare":
		return probeUnshare()
	case "ptrace":
		return probePtrace()
	case "raw-socket":
		return probeRawSocket()
	case "fork-pressure":
		return probeForkPressure(rest)
	case "tmpfs-quota":
		return probeTmpfsQuota(rest)
	case "dial":
		return probeDial(rest)
	case "proxy-get":
		return probeProxyGet(rest)
	case "proxy-connect":
		return probeProxyConnect(rest)
	case "network-visibility":
		return probeNetworkVisibility(rest)
	default:
		return emit(result{Probe: name, Allowed: false, Detail: "unknown probe"})
	}
}

// emit writes one JSON result line to stdout.
func emit(r result) int {
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		fmt.Fprintln(os.Stderr, "encode probe result:", err)
		return 2
	}
	return 0
}

// probeFSWrite attempts to create and write one file. It is used both to prove
// the read-only root filesystem rejects writes and to prove a writable tmpfs
// accepts them.
func probeFSWrite(path, name string) int {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return emit(result{Probe: name, Allowed: false, Detail: err.Error()})
	}
	_, werr := f.WriteString("aurora-probe")
	cerr := f.Close()
	if werr != nil {
		return emit(result{Probe: name, Allowed: false, Detail: werr.Error()})
	}
	if cerr != nil {
		return emit(result{Probe: name, Allowed: false, Detail: cerr.Error()})
	}
	_ = os.Remove(path)
	return emit(result{Probe: name, Allowed: true, Detail: "wrote " + path})
}

// probePrivilegeEscalation attempts to become root and inspects the effective
// capability and no-new-privileges state. Escalation is allowed only if a uid
// change succeeds, an effective capability remains, or no-new-privileges is off.
func probePrivilegeEscalation() int {
	escalated := false
	var details []string
	if err := syscall.Setuid(0); err == nil {
		escalated = true
		details = append(details, "setuid(0) succeeded")
	} else {
		details = append(details, "setuid(0): "+err.Error())
	}
	if err := syscall.Setgid(0); err == nil {
		escalated = true
		details = append(details, "setgid(0) succeeded")
	} else {
		details = append(details, "setgid(0): "+err.Error())
	}
	status := readProcStatus()
	if v, ok := status["CapEff"]; ok {
		details = append(details, "CapEff="+v)
		if strings.Trim(v, "0") != "" {
			escalated = true
		}
	}
	if v, ok := status["NoNewPrivs"]; ok {
		details = append(details, "NoNewPrivs="+v)
		if v != "1" {
			escalated = true
		}
	}
	return emit(result{Probe: "privilege-escalate", Allowed: escalated, Detail: strings.Join(details, "; ")})
}

// probeMount attempts a filesystem mount. The sandbox seccomp profile kills the
// process for this syscall, so a successful JSON result is unexpected.
func probeMount() int {
	err := syscall.Mount("tmpfs", "/tmp", "tmpfs", 0, "")
	return emit(result{Probe: "mount", Allowed: err == nil, Detail: errString(err)})
}

// probeUnshare attempts to create a new mount namespace.
func probeUnshare() int {
	_, _, errno := syscall.RawSyscall(syscall.SYS_UNSHARE, uintptr(syscall.CLONE_NEWNS), 0, 0)
	if errno != 0 {
		return emit(result{Probe: "unshare", Allowed: false, Detail: errno.Error()})
	}
	return emit(result{Probe: "unshare", Allowed: true, Detail: "created a new mount namespace"})
}

// probePtrace attempts to trace this process. PTRACE_TRACEME is request 0.
func probePtrace() int {
	_, _, errno := syscall.RawSyscall(syscall.SYS_PTRACE, 0, 0, 0)
	if errno != 0 {
		return emit(result{Probe: "ptrace", Allowed: false, Detail: errno.Error()})
	}
	return emit(result{Probe: "ptrace", Allowed: true, Detail: "traced this process"})
}

// probeRawSocket attempts to open a raw ICMP socket.
func probeRawSocket() int {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
	if err != nil {
		return emit(result{Probe: "raw-socket", Allowed: false, Detail: err.Error()})
	}
	_ = syscall.Close(fd)
	return emit(result{Probe: "raw-socket", Allowed: true, Detail: "created a raw ICMP socket"})
}

// probeForkPressure starts copies of itself until the PID limit stops it. It is
// allowed only if it reaches the configured limit, meaning the limit did not bind.
func probeForkPressure(args []string) int {
	limit := 256
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil {
			limit = n
		}
	}
	self, err := os.Executable()
	if err != nil {
		return emit(result{Probe: "fork-pressure", Allowed: false, Detail: err.Error()})
	}
	var children []*os.Process
	defer func() {
		for _, child := range children {
			_ = child.Kill()
		}
		for _, child := range children {
			_, _ = child.Wait()
		}
	}()
	count := 0
	for count < limit+16 {
		cmd := exec.Command(self, "serve")
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Start(); err != nil {
			return emit(result{
				Probe:   "fork-pressure",
				Allowed: count >= limit,
				Detail:  fmt.Sprintf("stopped after %d children at limit %d: %v", count, limit, err),
			})
		}
		children = append(children, cmd.Process)
		count++
	}
	return emit(result{
		Probe:   "fork-pressure",
		Allowed: count >= limit,
		Detail:  fmt.Sprintf("started %d children without hitting limit %d", count, limit),
	})
}

// probeTmpfsQuota writes one byte past a tmpfs quota and expects the write to
// stop with an error. Args are <path> <bytes>.
func probeTmpfsQuota(args []string) int {
	if len(args) < 2 {
		return emit(result{Probe: "tmpfs-quota", Allowed: false, Detail: "usage: tmpfs-quota <path> <bytes>"})
	}
	size, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || size <= 0 {
		return emit(result{Probe: "tmpfs-quota", Allowed: false, Detail: "invalid size " + args[1]})
	}
	f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return emit(result{Probe: "tmpfs-quota", Allowed: false, Detail: "open: " + err.Error()})
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	var written int64
	for written <= size {
		n, werr := f.Write(buf)
		written += int64(n)
		if werr != nil {
			_ = os.Remove(args[0])
			return emit(result{
				Probe:   "tmpfs-quota",
				Allowed: false,
				Detail:  fmt.Sprintf("stopped at %d bytes past quota %d: %v", written, size, werr),
			})
		}
	}
	_ = os.Remove(args[0])
	return emit(result{
		Probe:   "tmpfs-quota",
		Allowed: true,
		Detail:  fmt.Sprintf("wrote %d bytes past quota %d", written, size),
	})
}

// probeDial attempts one direct TCP connection.
func probeDial(args []string) int {
	if len(args) < 1 {
		return emit(result{Probe: "dial", Allowed: false, Detail: "usage: dial <host:port>"})
	}
	conn, err := net.DialTimeout("tcp", args[0], defaultDial)
	if err != nil {
		return emit(result{Probe: "dial", Allowed: false, Detail: args[0] + ": " + err.Error()})
	}
	_ = conn.Close()
	return emit(result{Probe: "dial", Allowed: true, Detail: "connected to " + args[0]})
}

// probeProxyGet sends one plain HTTP absolute-form request through the egress
// sidecar. It is allowed only for a 2xx response, so the exact configured
// Multica origin succeeds while every other target is refused by the proxy.
func probeProxyGet(args []string) int {
	if len(args) < 1 {
		return emit(result{Probe: "proxy-get", Allowed: false, Detail: "usage: proxy-get <url>"})
	}
	client := proxyClient()
	resp, err := client.Get(args[0])
	if err != nil {
		return emit(result{Probe: "proxy-get", Allowed: false, Detail: args[0] + ": " + err.Error()})
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return emit(result{
		Probe:   "proxy-get",
		Allowed: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Detail:  fmt.Sprintf("%s -> %d", args[0], resp.StatusCode),
	})
}

// probeProxyConnect sends one CONNECT request through the egress sidecar. A
// refusal by the proxy surfaces as a client error, so only a completed request
// counts as allowed.
func probeProxyConnect(args []string) int {
	if len(args) < 1 {
		return emit(result{Probe: "proxy-connect", Allowed: false, Detail: "usage: proxy-connect <url>"})
	}
	client := proxyClient()
	resp, err := client.Get(args[0])
	if err != nil {
		return emit(result{Probe: "proxy-connect", Allowed: false, Detail: args[0] + ": " + err.Error()})
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return emit(result{Probe: "proxy-connect", Allowed: true, Detail: fmt.Sprintf("%s -> %d", args[0], resp.StatusCode)})
}

// probeNetworkVisibility attempts to reach another workspace's container by
// service name and by address. Args are <name> <ip> [port].
func probeNetworkVisibility(args []string) int {
	if len(args) < 2 {
		return emit(result{Probe: "network-visibility", Allowed: false, Detail: "usage: network-visibility <name> <ip> [port]"})
	}
	port := "9000"
	if len(args) > 2 {
		port = args[2]
	}
	for _, host := range []string{args[0], args[1]} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), defaultDial)
		if err == nil {
			_ = conn.Close()
			return emit(result{Probe: "network-visibility", Allowed: true, Detail: "reached " + host + ":" + port})
		}
	}
	return emit(result{Probe: "network-visibility", Allowed: false, Detail: "could not reach the other workspace container by name or address"})
}

// probeListen runs the fixture image as a peer container that accepts one
// connection, so the isolation test can prove the sandbox cannot reach it.
func probeListen(args []string) int {
	port := "9000"
	if len(args) > 0 && args[0] != "" {
		port = args[0]
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 2
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return 2
		}
		go func(c net.Conn) {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}(conn)
	}
}

// proxyClient builds an HTTP client that always uses the sandbox's configured
// egress sidecar, never the environment's implicit bypass rules.
func proxyClient() *http.Client {
	raw := os.Getenv("HTTP_PROXY")
	if raw == "" {
		raw = "http://egress:3128"
	}
	proxyURL, err := url.Parse(raw)
	if err != nil {
		proxyURL, _ = url.Parse("http://egress:3128")
	}
	return &http.Client{
		Timeout: proxyTimeout,
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: defaultDial,
		},
	}
}

// readProcStatus parses /proc/self/status into a key/value map for the
// capability and no-new-privileges assertions.
func readProcStatus() map[string]string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil
	}
	status := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		status[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return status
}

func errString(err error) string {
	if err == nil {
		return "operation succeeded"
	}
	return err.Error()
}
