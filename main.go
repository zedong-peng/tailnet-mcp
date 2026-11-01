// Command tailnet-mcp bridges a stdio MCP client to a Streamable HTTP MCP server that is only
// reachable inside a tailnet. It embeds a Tailscale node (tsnet), so it needs no root, no TUN
// device and no tailscaled: a host that can only launch MCP servers as commands (a sandboxed
// agent computer, for example) can still reach a server published with `tailscale serve`.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

func main() {
	url := flag.String("url", "", "MCP endpoint inside the tailnet, e.g. https://host.tailnet.ts.net:8443/mcp")
	hostname := flag.String("hostname", "tailnet-mcp", "node name in the tailnet")
	stateDir := flag.String("state", "", "tsnet state directory (default ~/.local/state/tailnet-mcp/<hostname>)")
	check := flag.Bool("check", false, "connect, list the server's tools on stderr, and exit")
	verbose := flag.Bool("v", false, "log tsnet internals to stderr")
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("tailnet-mcp: ")
	if *url == "" {
		log.Fatal("-url is required")
	}
	if *stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatal(err)
		}
		*stateDir = filepath.Join(home, ".local", "state", "tailnet-mcp", *hostname)
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		log.Fatal(err)
	}

	// stdout carries MCP frames, so every log line goes to stderr.
	srv := &tsnet.Server{
		Dir:      *stateDir,
		Hostname: *hostname,
		AuthKey:  os.Getenv("TS_AUTHKEY"), // only needed for the first login; the node key is kept in Dir
		UserLogf: log.Printf,
		Logf:     func(string, ...any) {},
	}
	if *verbose {
		srv.Logf = log.Printf
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	if _, err := srv.Up(ctx); err != nil {
		log.Fatalf("joining tailnet: %v", err)
	}
	cancel()
	waitForPeer(srv, *url)

	// A peer the ACL drops never answers, so bound the dial instead of hanging.
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return srv.Dial(ctx, network, addr)
		},
		ForceAttemptHTTP2: true,
	}}
	b := &bridge{url: *url, http: client, out: bufio.NewWriter(os.Stdout)}
	if *check {
		if err := b.check(); err != nil {
			log.Fatal(err)
		}
		return
	}
	b.run(os.Stdin)
}

type bridge struct {
	url  string
	http *http.Client

	mu        sync.Mutex // guards session, protocol, initFrame
	session   string
	protocol  string
	initFrame []byte     // the client's initialize, replayed when the server forgets the session
	reinitMu  sync.Mutex // one re-initialization at a time
	outMu     sync.Mutex
	out       *bufio.Writer
	inflight  sync.WaitGroup
}

// message holds just the JSON-RPC fields the bridge needs to look at.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
}

func (b *bridge) run(in io.Reader) {
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			// Everything after initialize needs the session id it returns, so it goes first, alone.
			var m message
			if json.Unmarshal(line, &m) == nil && m.Method == "initialize" {
				b.inflight.Wait()
				b.forward(line)
				continue
			}
			b.inflight.Add(1)
			go func(frame []byte) {
				defer b.inflight.Done()
				b.forward(frame)
			}(line)
		}
		if err != nil {
			break // EOF: the client is done
		}
	}
	b.inflight.Wait()
	b.endSession()
}

// forward POSTs one client frame and writes whatever the server answers back to stdout.
func (b *bridge) forward(frame []byte) {
	var m message
	_ = json.Unmarshal(frame, &m)
	isRequest := len(m.ID) > 0 && m.Method != ""

	if m.Method == "initialize" {
		b.mu.Lock()
		b.initFrame = frame
		b.mu.Unlock()
	}

	resp, err := b.send(frame)
	if err == nil && m.Method != "initialize" && sessionLost(resp) {
		// The server restarted and dropped our session: start a new one and resend.
		resp.Body.Close()
		if err = b.reinit(resp.Request.Header.Get("Mcp-Session-Id")); err == nil {
			resp, err = b.send(frame)
		}
	}
	if err != nil {
		b.fail(m, isRequest, err)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent:
		return
	case resp.StatusCode >= 300:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		b.fail(m, isRequest, fmt.Errorf("server returned %s: %s", resp.Status, bytes.TrimSpace(body)))
		return
	}

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		err = readSSE(resp.Body, func(data []byte) { b.emit(m, data) })
	} else {
		var body []byte
		if body, err = io.ReadAll(resp.Body); err == nil && len(bytes.TrimSpace(body)) > 0 {
			b.emit(m, body)
		}
	}
	if err != nil {
		b.fail(m, isRequest, err)
	}
}

// send POSTs a frame, retrying dials that never reached the server, and records the session id.
func (b *bridge) send(frame []byte) (resp *http.Response, err error) {
	for attempt := 0; attempt < 3; attempt++ {
		if resp, err = b.post(frame); err == nil || !isDialError(err) {
			break
		}
		log.Printf("%v, retrying", err)
	}
	if err == nil {
		if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
			b.mu.Lock()
			b.session = id
			b.mu.Unlock()
		}
	}
	return resp, err
}

// sessionLost reports whether the server no longer knows the session the request carried.
// It peeks at a 400 body, so only call it before the body is otherwise read.
func sessionLost(resp *http.Response) bool {
	if resp.Request.Header.Get("Mcp-Session-Id") == "" {
		return false
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return true
	case http.StatusBadRequest:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return bytes.Contains(bytes.ToLower(body), []byte("session"))
	}
	return false
}

// reinit replays the client's initialize and initialized frames to get a fresh session,
// unless another request already did so after stale was rejected.
func (b *bridge) reinit(stale string) error {
	b.reinitMu.Lock()
	defer b.reinitMu.Unlock()
	b.mu.Lock()
	init, current := b.initFrame, b.session
	if current == stale {
		b.session = ""
	}
	b.mu.Unlock()
	if current != stale {
		return nil
	}
	if init == nil {
		return errors.New("server lost the session before initialize was seen")
	}
	log.Printf("server lost the session, re-initializing")
	resp, err := b.send(init)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body) // the client already has an initialize result
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("re-initialize: server returned %s", resp.Status)
	}
	if resp, err = b.send([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (b *bridge) post(frame []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, b.url, bytes.NewReader(frame))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	b.mu.Lock()
	if b.session != "" {
		req.Header.Set("Mcp-Session-Id", b.session)
	}
	if b.protocol != "" {
		req.Header.Set("Mcp-Protocol-Version", b.protocol)
	}
	b.mu.Unlock()
	return b.http.Do(req)
}

// isDialError reports whether the request never reached the server, so it is safe to resend.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// waitForPeer pings the server's node until it answers. Right after this node starts, the peer
// may still hold a stale path to it (seen after a clean restart), and the first dial would time out.
func waitForPeer(srv *tsnet.Server, rawURL string) {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	lc, err := srv.LocalClient()
	if err != nil {
		return
	}
	deadline := time.Now().Add(60 * time.Second)
	logged := false
	for time.Now().Before(deadline) {
		if ip, ok := peerIP(lc, host); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			res, err := lc.Ping(ctx, ip, tailcfg.PingTSMP)
			cancel()
			if err == nil && res.Err == "" {
				return
			}
		}
		if !logged {
			log.Printf("waiting for %s to answer", host)
			logged = true
		}
		time.Sleep(time.Second)
	}
	log.Printf("%s did not answer pings; trying anyway", host)
}

// peerIP finds host (a MagicDNS name or a Tailscale IP) among this node's peers.
func peerIP(lc *local.Client, host string) (netip.Addr, bool) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := lc.Status(ctx)
	if err != nil {
		return netip.Addr{}, false
	}
	for _, p := range st.Peer {
		name := strings.TrimSuffix(p.DNSName, ".")
		if (name == host || strings.SplitN(name, ".", 2)[0] == host) && len(p.TailscaleIPs) > 0 {
			return p.TailscaleIPs[0], true
		}
	}
	return netip.Addr{}, false
}

// emit writes one server frame to stdout, remembering the negotiated protocol version.
func (b *bridge) emit(sent message, data []byte) {
	if sent.Method == "initialize" {
		var r struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if json.Unmarshal(data, &r) == nil && r.Result.ProtocolVersion != "" {
			b.mu.Lock()
			b.protocol = r.Result.ProtocolVersion
			b.mu.Unlock()
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, data) != nil {
		return
	}
	b.outMu.Lock()
	defer b.outMu.Unlock()
	b.out.Write(compact.Bytes())
	b.out.WriteByte('\n')
	b.out.Flush()
}

// fail answers a request with a JSON-RPC error so the client does not wait forever.
func (b *bridge) fail(m message, isRequest bool, err error) {
	log.Printf("%s: %v", orDefault(m.Method, "frame"), err)
	if !isRequest {
		return
	}
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      m.ID,
		"error":   map[string]any{"code": -32000, "message": "tailnet-mcp: " + err.Error()},
	})
	b.emit(message{}, resp)
}

func (b *bridge) endSession() {
	b.mu.Lock()
	id := b.session
	b.mu.Unlock()
	if id == "" {
		return
	}
	req, _ := http.NewRequest(http.MethodDelete, b.url, nil)
	req.Header.Set("Mcp-Session-Id", id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if resp, err := b.http.Do(req.WithContext(ctx)); err == nil {
		resp.Body.Close()
	}
}

// readSSE calls fn with the data of each server-sent event.
func readSSE(r io.Reader, fn func([]byte)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	var data [][]byte
	flush := func() {
		if len(data) > 0 {
			fn(bytes.Join(data, []byte("\n")))
			data = nil
		}
	}
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			flush()
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
		}
	}
	flush()
	return sc.Err()
}

// check runs initialize + tools/list through the bridge and prints the tool names.
func (b *bridge) check() error {
	var buf bytes.Buffer
	b.out = bufio.NewWriter(&buf)
	call := func(frame string) (json.RawMessage, error) {
		buf.Reset()
		b.forward([]byte(frame)) // synchronous: the answer is in buf when it returns
		var r struct {
			Result json.RawMessage           `json:"result"`
			Error  *struct{ Message string } `json:"error"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &r); err != nil {
			return nil, errors.New("no response from server")
		}
		if r.Error != nil {
			return nil, errors.New(strings.TrimPrefix(r.Error.Message, "tailnet-mcp: "))
		}
		return r.Result, nil
	}
	defer b.endSession()

	res, err := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"tailnet-mcp-check","version":"1"}}}`)
	if err != nil {
		return err
	}
	var init struct {
		ServerInfo struct{ Name, Version string } `json:"serverInfo"`
	}
	_ = json.Unmarshal(res, &init)
	log.Printf("connected to %s %s", init.ServerInfo.Name, init.ServerInfo.Version)

	b.forward([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if res, err = call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); err != nil {
		return err
	}
	var list struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	_ = json.Unmarshal(res, &list)
	names := make([]string, len(list.Tools))
	for i, t := range list.Tools {
		names[i] = t.Name
	}
	log.Printf("tools: %s", strings.Join(names, ", "))
	return nil
}

func orDefault(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
