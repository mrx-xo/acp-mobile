package main

import (
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// Grouping responses ahead of notifications moves records across the explicit
// replay boundary. The browser must see the exact order the proxy sent.
func TestReplayBridgePreservesBoundariesAndRecordOrder(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "acp-replay-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "peer.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	frames := []string{
		`{"jsonrpc":"2.0","method":"acp-multiplex/replay_start"}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","text":"history"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"fs/read_text_file","params":{"path":"/nonexistent/replay-fixture"}}`,
		`{"jsonrpc":"2.0","id":0,"result":{"sessionId":"test"}}`,
		`{"jsonrpc":"2.0","method":"acp-multiplex/replay_complete"}`,
		`{"jsonrpc":"2.0","id":1,"result":{"stopReason":"end_turn"}}`,
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		payload := strings.Join(frames, "\n") + "\n"
		// Split inside the start marker to exercise NDJSON framing too.
		io.WriteString(conn, payload[:17])
		io.WriteString(conn, payload[17:])
		io.Copy(io.Discard, conn)
	}()
	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		bridgeWebSocket(ws, path)
	}))
	t.Cleanup(srv.Close)
	ws, err := websocket.Dial(strings.Replace(srv.URL, "http:", "ws:", 1), "", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for i, want := range frames {
		var got string
		if err := websocket.Message.Receive(ws, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("frame %d = %s, want %s", i, got, want)
		}
	}
}
