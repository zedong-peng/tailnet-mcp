package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeServer is a minimal Streamable HTTP MCP server: it hands out a session id on initialize,
// answers requests as JSON or SSE, and can be told to forget every session (a restart).
type fakeServer struct {
	mu       sync.Mutex
	sessions map[string]bool
	next     int
	sse      bool
	inits    int
	deleted  []string
}

func newFakeServer(sse bool) (*fakeServer, *httptest.Server) {
	f := &fakeServer{sessions: map[string]bool{}, sse: sse}
	return f, httptest.NewServer(f)
}

func (f *fakeServer) restart() {
	f.mu.Lock()
	f.sessions = map[string]bool{}
	f.mu.Unlock()
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sid := r.Header.Get("Mcp-Session-Id")
	if r.Method == http.MethodDelete {
		f.deleted = append(f.deleted, sid)
		delete(f.sessions, sid)
		return
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &m)

	if m.Method == "initialize" {
		f.next++
		f.inits++
		sid = fmt.Sprintf("s%d", f.next)
		f.sessions[sid] = true
		w.Header().Set("Mcp-Session-Id", sid)
	} else if !f.sessions[sid] {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}
	if len(m.ID) == 0 { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result := map[string]any{"method": m.Method, "session": sid}
	if m.Method == "initialize" {
		result = map[string]any{"protocolVersion": "2025-06-18", "serverInfo": map[string]string{"name": "fake", "version": "1"}}
	}
	resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

func newBridge(url string) (*bridge, *bytes.Buffer) {
	var out bytes.Buffer
	return &bridge{url: url, http: http.DefaultClient, out: bufio.NewWriter(&out)}, &out
}

// lines decodes the newline-delimited frames the bridge wrote to stdout.
func lines(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q", l)
		}
		frames = append(frames, m)
	}
	return frames
}

const (
	initFrame      = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	initedFrame    = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	toolsListFrame = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
)

func TestRunJSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			f, srv := newFakeServer(sse)
			defer srv.Close()
			b, out := newBridge(srv.URL)

			b.run(strings.NewReader(initFrame + "\n" + initedFrame + "\n" + toolsListFrame + "\n"))

			frames := lines(t, out)
			if len(frames) != 2 {
				t.Fatalf("got %d frames, want 2 (initialize and tools/list results): %v", len(frames), frames)
			}
			if got := frames[1]["result"].(map[string]any)["session"]; got != "s1" {
				t.Errorf("tools/list carried session %v, want s1", got)
			}
			if b.protocol != "2025-06-18" {
				t.Errorf("protocol = %q, want the negotiated 2025-06-18", b.protocol)
			}
			if len(f.deleted) != 1 || f.deleted[0] != "s1" {
				t.Errorf("session not ended on EOF: deleted %v", f.deleted)
			}
		})
	}
}

func TestReinitializesWhenServerForgetsSession(t *testing.T) {
	f, srv := newFakeServer(false)
	defer srv.Close()
	b, out := newBridge(srv.URL)

	b.forward([]byte(initFrame))
	b.forward([]byte(initedFrame))
	f.restart()
	b.forward([]byte(toolsListFrame))

	frames := lines(t, out)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2: %v", len(frames), frames)
	}
	last := frames[1]
	if last["error"] != nil {
		t.Fatalf("tools/list failed after restart: %v", last["error"])
	}
	if got := last["result"].(map[string]any)["session"]; got != "s2" {
		t.Errorf("retried on session %v, want the fresh s2", got)
	}
	if f.inits != 2 {
		t.Errorf("server saw %d initializes, want 2", f.inits)
	}
}

func TestServerErrorBecomesJSONRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	b, out := newBridge(srv.URL)

	b.forward([]byte(toolsListFrame))
	b.forward([]byte(initedFrame)) // a notification gets no reply, even on error

	frames := lines(t, out)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1: %v", len(frames), frames)
	}
	e, _ := frames[0]["error"].(map[string]any)
	if e == nil || frames[0]["id"] != float64(2) || !strings.Contains(e["message"].(string), "403") {
		t.Errorf("want a JSON-RPC error for id 2 mentioning 403, got %v", frames[0])
	}
}

func TestReadSSE(t *testing.T) {
	stream := "event: message\ndata: {\"a\":1}\n\n: comment\ndata: {\"b\":\ndata: 2}\n\ndata: {\"c\":3}"
	var got []string
	if err := readSSE(strings.NewReader(stream), func(d []byte) { got = append(got, string(d)) }); err != nil {
		t.Fatal(err)
	}
	want := []string{`{"a":1}`, "{\"b\":\n2}", `{"c":3}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("events = %q, want %q", got, want)
	}
}

func TestSessionLost(t *testing.T) {
	mk := func(status int, body, sid string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Request: req}
	}
	cases := []struct {
		resp *http.Response
		want bool
	}{
		{mk(404, "", "s1"), true},
		{mk(400, `{"error":"Bad Request: No valid session ID provided"}`, "s1"), true},
		{mk(400, `{"error":"invalid params"}`, "s1"), false},
		{mk(404, "", ""), false}, // no session was sent, so none was lost
		{mk(200, "", "s1"), false},
	}
	for i, c := range cases {
		if got := sessionLost(c.resp); got != c.want {
			t.Errorf("case %d: sessionLost = %v, want %v", i, got, c.want)
		}
	}
	// the peeked body must still be readable afterwards
	r := mk(400, "session gone", "s1")
	sessionLost(r)
	if b, _ := io.ReadAll(r.Body); string(b) != "session gone" {
		t.Errorf("body after peek = %q", b)
	}
}

func TestIsDialError(t *testing.T) {
	if !isDialError(fmt.Errorf("wrapped: %w", &net.OpError{Op: "dial", Err: errors.New("timeout")})) {
		t.Error("dial error not recognized")
	}
	if isDialError(&net.OpError{Op: "read", Err: errors.New("reset")}) {
		t.Error("read error treated as a dial error; resending could repeat a tool call")
	}
}
