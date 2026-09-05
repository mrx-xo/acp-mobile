package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/websocket"
)

func TestCurrentStatusesMergesPhoneTurns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".acp-mobile"), 0700); err != nil {
		t.Fatal(err)
	}
	sidecar := []byte(`{"idle-session":"idle","permission-session":"permission"}`)
	if err := os.WriteFile(filepath.Join(home, ".acp-mobile", "status.json"), sidecar, 0600); err != nil {
		t.Fatal(err)
	}

	phoneTurns.mu.Lock()
	previous := phoneTurns.m
	phoneTurns.m = map[string]int{
		"phone-session":      1,
		"permission-session": 1,
	}
	phoneTurns.mu.Unlock()
	t.Cleanup(func() {
		phoneTurns.mu.Lock()
		phoneTurns.m = previous
		phoneTurns.mu.Unlock()
	})

	statuses := currentStatuses()
	if got := statuses["idle-session"]; got != "idle" {
		t.Fatalf("idle sidecar status = %q, want idle", got)
	}
	if got := statuses["phone-session"]; got != "busy" {
		t.Fatalf("phone turn status = %q, want busy", got)
	}
	if got := statuses["permission-session"]; got != "permission" {
		t.Fatalf("permission status = %q, want permission precedence", got)
	}
}

func TestHandleStatusesIsUncached(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	req := httptest.NewRequest(http.MethodPost, "/api/statuses", nil)
	w := httptest.NewRecorder()
	handleStatuses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var payload struct {
		Statuses map[string]string `json:"statuses"`
		Version  string            `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Statuses == nil {
		t.Fatal("statuses should be an empty object, not null")
	}
	if payload.Version != buildID {
		t.Fatalf("version = %q, want %q", payload.Version, buildID)
	}
}

// TestToolReplayIntegration verifies that tool_call and tool_call_update messages
// are correctly relayed through the WebSocket bridge, with proper title, _meta,
// rawInput, and content fields preserved.
//
// This is a full integration test that starts a fake acp-multiplex socket,
// an HTTP server with WebSocket handler, and connects as a browser client.
//
// Skip by default: run in a separate VM to avoid disrupting work.
//
//	go test -run TestToolReplayIntegration -count=1
func TestToolReplayIntegration(t *testing.T) {
	t.Skip("Integration test: run in a separate VM to avoid disrupting work. Use: go test -run TestToolReplayIntegration -count=1 -v")

	tmpDir := t.TempDir()
	sockDir := filepath.Join(tmpDir, "acp-multiplex")
	os.MkdirAll(sockDir, 0700)
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	// Build replay messages
	messages := buildTestReplay("test-session-1")

	// Start fake acp-multiplex socket server
	sockPath := filepath.Join(sockDir, "99999.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			for _, m := range messages {
				conn.Write(m)
				conn.Write([]byte("\n"))
			}
			// Keep connection open for bridging
			buf := make([]byte, 4096)
			for {
				conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				_, err := conn.Read(buf)
				if err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	// Start HTTP server with WebSocket handler
	testMode = true
	mux := http.NewServeMux()
	mux.Handle("/ws", &websocket.Server{
		Handler: func(ws *websocket.Conn) {
			bridgeWebSocket(ws, sockPath)
		},
		Handshake: func(config *websocket.Config, r *http.Request) error {
			return nil // skip origin check in test
		},
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Connect as WebSocket client
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/ws?sock=99999"
	ws, err := websocket.Dial(wsURL, "", srv.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	// Collect all messages
	var received []json.RawMessage
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var msg string
		err := websocket.Message.Receive(ws, &msg)
		if err != nil {
			break
		}
		received = append(received, json.RawMessage(msg))
	}

	if len(received) == 0 {
		t.Fatal("received no messages")
	}
	t.Logf("received %d messages", len(received))

	// Parse and verify tool_call messages
	type toolCall struct {
		ID       string
		Title    string
		ToolName string // from _meta.claudeCode.toolName
		Status   string
		Content  string
	}
	tools := make(map[string]*toolCall)

	for _, raw := range received {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Update json.RawMessage `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Method != "session/update" {
			continue
		}
		var update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			ToolCallId    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Status        string          `json:"status"`
			Meta          json.RawMessage `json:"_meta"`
			RawInput      json.RawMessage `json:"rawInput"`
			Content       json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(msg.Params.Update, &update); err != nil {
			continue
		}

		switch update.SessionUpdate {
		case "tool_call":
			tc := tools[update.ToolCallId]
			if tc == nil {
				tc = &toolCall{ID: update.ToolCallId}
				tools[update.ToolCallId] = tc
			}
			if update.Title != "" {
				tc.Title = update.Title
			}
			// Extract _meta.claudeCode.toolName
			if len(update.Meta) > 0 {
				var meta struct {
					ClaudeCode struct {
						ToolName string `json:"toolName"`
					} `json:"claudeCode"`
				}
				json.Unmarshal(update.Meta, &meta)
				if meta.ClaudeCode.ToolName != "" {
					tc.ToolName = meta.ClaudeCode.ToolName
				}
			}
		case "tool_call_update":
			tc := tools[update.ToolCallId]
			if tc == nil {
				tc = &toolCall{ID: update.ToolCallId}
				tools[update.ToolCallId] = tc
			}
			tc.Status = update.Status
			// Extract content.text (object form)
			if len(update.Content) > 0 {
				var obj struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(update.Content, &obj) == nil && obj.Text != "" {
					tc.Content = obj.Text
				}
			}
		}
	}

	// Expected tools with their properties
	expected := []struct {
		id       string
		title    string
		toolName string
		status   string
		content  string
	}{
		{"tc1", `grep "authenticate" src/`, "Grep", "completed", "Found 5 matches in src/auth/"},
		{"tc2", "Read src/auth/handler.go", "Read", "completed", "package auth\n\nfunc HandleLogin(...)"},
		{"tc3", "Read src/auth/middleware.go", "Read", "completed", "package auth\n\nfunc AuthMiddleware(...)"},
		{"tc4", "Read src/auth/handler.go", "Read", "completed", "package auth\n\nfunc HandleLogin(...)"},
		{"tc5", "Edit src/auth/handler.go", "Edit", "completed", "File edited successfully"},
		{"tc6", "Run tests", "Bash", "completed", "ok  src/auth  0.5s"},
		{"tc7", "Build project", "Bash", "completed", "Build successful"},
		{"tc8", "Read src/auth/handler_test.go", "Read", "completed", "package auth\n\nfunc TestHandleLogin(t *testing.T) {"},
		{"tc9", "Edit src/auth/handler_test.go", "Edit", "completed", "File edited successfully"},
		{"tc10", "Run tests with verbose output", "Bash", "completed", "=== RUN TestHandleLoginError\n--- PASS: TestHandleLoginError\nPASS"},
	}

	for _, exp := range expected {
		tc, ok := tools[exp.id]
		if !ok {
			t.Errorf("tool %s: not found in replay", exp.id)
			continue
		}
		if tc.Title != exp.title {
			t.Errorf("tool %s title: got %q, want %q", exp.id, tc.Title, exp.title)
		}
		if tc.ToolName != exp.toolName {
			t.Errorf("tool %s toolName (_meta.claudeCode): got %q, want %q", exp.id, tc.ToolName, exp.toolName)
		}
		if tc.Status != exp.status {
			t.Errorf("tool %s status: got %q, want %q", exp.id, tc.Status, exp.status)
		}
		if tc.Content != exp.content {
			t.Errorf("tool %s content: got %q, want %q", exp.id, tc.Content, exp.content)
		}
	}

	// Verify no tools have empty title or toolName (the "?" bug)
	for id, tc := range tools {
		if tc.Title == "" && tc.ToolName == "" {
			t.Errorf("tool %s: both title and toolName empty — would render as '?'", id)
		}
		if tc.Content == "" {
			t.Errorf("tool %s (%s): content empty — output would not display", id, tc.ToolName)
		}
	}
}

// buildTestReplay constructs a realistic set of replay messages simulating
// a multi-turn conversation with various tool calls (Grep, Read, Edit, Bash).
func buildTestReplay(sessionID string) [][]byte {
	msg := func(v interface{}) []byte {
		b, _ := json.Marshal(v)
		return b
	}

	update := func(kind string, extra map[string]interface{}) []byte {
		u := map[string]interface{}{"sessionUpdate": kind}
		for k, v := range extra {
			u[k] = v
		}
		return msg(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]interface{}{
				"sessionId": sessionID,
				"update":    u,
			},
		})
	}

	toolCall := func(id, title, toolName string, rawInput map[string]string) []byte {
		return update("tool_call", map[string]interface{}{
			"toolCallId": id,
			"title":      title,
			"_meta":      map[string]interface{}{"claudeCode": map[string]string{"toolName": toolName}},
			"rawInput":   rawInput,
		})
	}

	toolResult := func(id, toolName, text string) []byte {
		return update("tool_call_update", map[string]interface{}{
			"toolCallId": id,
			"status":     "completed",
			"_meta":      map[string]interface{}{"claudeCode": map[string]string{"toolName": toolName}},
			"content":    map[string]string{"type": "text", "text": text},
		})
	}

	agentMsg := func(text string) []byte {
		return update("agent_message_chunk", map[string]interface{}{
			"content": map[string]string{"type": "text", "text": text},
		})
	}

	userMsg := func(text string) []byte {
		return update("user_message_chunk", map[string]interface{}{
			"content": map[string]string{"type": "text", "text": text},
		})
	}

	turnComplete := func() []byte { return update("turn_complete", nil) }

	var messages [][]byte

	// Meta
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "method": "acp-multiplex/meta",
		"params": map[string]interface{}{"name": "TEST: Tool Replay"},
	}))

	// Init + session/new responses
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(`0`),
		"result": map[string]interface{}{
			"protocolVersion": 1,
			"agentInfo":       map[string]string{"name": "test-agent", "title": "Test Agent"},
		},
	}))
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(`0`),
		"result": map[string]interface{}{"sessionId": sessionID},
	}))

	// Turn 1: search + read
	messages = append(messages, userMsg("Search the codebase for authentication handling"))
	messages = append(messages, agentMsg("Let me search for authentication code."))
	messages = append(messages, toolCall("tc1", `grep "authenticate" src/`, "Grep", map[string]string{"pattern": "authenticate", "path": "src/"}))
	messages = append(messages, toolResult("tc1", "Grep", "Found 5 matches in src/auth/"))
	messages = append(messages, toolCall("tc2", "Read src/auth/handler.go", "Read", map[string]string{"file_path": "src/auth/handler.go"}))
	messages = append(messages, toolResult("tc2", "Read", "package auth\n\nfunc HandleLogin(...)"))
	messages = append(messages, toolCall("tc3", "Read src/auth/middleware.go", "Read", map[string]string{"file_path": "src/auth/middleware.go"}))
	messages = append(messages, toolResult("tc3", "Read", "package auth\n\nfunc AuthMiddleware(...)"))
	messages = append(messages, agentMsg("I found the authentication code. Here's what I see..."))
	messages = append(messages, turnComplete())

	// Turn 2: fix bug with edit + bash
	messages = append(messages, userMsg("Now fix the bug in the login handler"))
	messages = append(messages, agentMsg("I'll fix the login handler bug."))
	messages = append(messages, toolCall("tc4", "Read src/auth/handler.go", "Read", map[string]string{"file_path": "src/auth/handler.go"}))
	messages = append(messages, toolResult("tc4", "Read", "package auth\n\nfunc HandleLogin(...)"))
	messages = append(messages, toolCall("tc5", "Edit src/auth/handler.go", "Edit", map[string]string{
		"file_path": "src/auth/handler.go", "old_string": "if err != nil {", "new_string": "if err != nil {\n\tlog.Error(err)",
	}))
	messages = append(messages, toolResult("tc5", "Edit", "File edited successfully"))
	messages = append(messages, toolCall("tc6", "Run tests", "Bash", map[string]string{"command": "go test ./src/auth/..."}))
	messages = append(messages, toolResult("tc6", "Bash", "ok  src/auth  0.5s"))
	messages = append(messages, toolCall("tc7", "Build project", "Bash", map[string]string{"command": "go build ./..."}))
	messages = append(messages, toolResult("tc7", "Bash", "Build successful"))
	messages = append(messages, agentMsg("The bug is fixed. Tests pass and the build succeeds."))
	messages = append(messages, turnComplete())

	// Turn 3: add test
	messages = append(messages, userMsg("Now add a test for the fix"))
	messages = append(messages, agentMsg("I'll add a test for the login fix."))
	messages = append(messages, toolCall("tc8", "Read src/auth/handler_test.go", "Read", map[string]string{"file_path": "src/auth/handler_test.go"}))
	messages = append(messages, toolResult("tc8", "Read", "package auth\n\nfunc TestHandleLogin(t *testing.T) {"))
	messages = append(messages, toolCall("tc9", "Edit src/auth/handler_test.go", "Edit", map[string]string{
		"file_path": "src/auth/handler_test.go", "old_string": "func TestHandleLogin", "new_string": "func TestHandleLoginError",
	}))
	messages = append(messages, toolResult("tc9", "Edit", "File edited successfully"))
	messages = append(messages, toolCall("tc10", "Run tests with verbose output", "Bash", map[string]string{"command": "go test -v ./src/auth/..."}))
	messages = append(messages, toolResult("tc10", "Bash", "=== RUN TestHandleLoginError\n--- PASS: TestHandleLoginError\nPASS"))
	messages = append(messages, agentMsg("Test added and passing. All done."))
	messages = append(messages, turnComplete())

	return messages
}

// TestToolReplayMessageFormat verifies the structure of replay messages
// without starting servers. This is a fast unit test.
func TestToolReplayMessageFormat(t *testing.T) {
	messages := buildTestReplay("test-session-1")

	type toolInfo struct {
		id       string
		title    string
		toolName string
		content  string
	}
	var tools []toolInfo

	for _, raw := range messages {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Update struct {
					SessionUpdate string          `json:"sessionUpdate"`
					ToolCallId    string          `json:"toolCallId"`
					Title         string          `json:"title"`
					Meta          json.RawMessage `json:"_meta"`
					Content       json.RawMessage `json:"content"`
				} `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Method != "session/update" {
			continue
		}
		u := msg.Params.Update

		switch u.SessionUpdate {
		case "tool_call":
			if u.Title == "" {
				t.Errorf("tool_call %s: missing title field", u.ToolCallId)
			}
			// Check _meta.claudeCode.toolName exists
			if len(u.Meta) > 0 {
				var meta struct {
					ClaudeCode struct {
						ToolName string `json:"toolName"`
					} `json:"claudeCode"`
				}
				json.Unmarshal(u.Meta, &meta)
				if meta.ClaudeCode.ToolName == "" {
					t.Errorf("tool_call %s: missing _meta.claudeCode.toolName", u.ToolCallId)
				}
				tools = append(tools, toolInfo{
					id: u.ToolCallId, title: u.Title, toolName: meta.ClaudeCode.ToolName,
				})
			}

		case "tool_call_update":
			// Verify content is extractable (object with .text)
			if len(u.Content) > 0 {
				var obj struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(u.Content, &obj); err != nil || obj.Text == "" {
					t.Errorf("tool_call_update %s: content not extractable as {text}: %s", u.ToolCallId, string(u.Content))
				}
				// Find matching tool and record content
				for i := range tools {
					if tools[i].id == u.ToolCallId {
						tools[i].content = obj.Text
					}
				}
			}
		}
	}

	// Every tool should have title, toolName, and content
	for _, tc := range tools {
		if tc.title == "" {
			t.Errorf("tool %s: no title (would show toolName as fallback)", tc.id)
		}
		if tc.toolName == "" {
			t.Errorf("tool %s: no toolName (icon selection and Edit/Write detection would fail)", tc.id)
		}
		if tc.content == "" {
			t.Errorf("tool %s (%s): no content (output would not display)", tc.id, tc.toolName)
		}
	}

	// Verify tool types used
	typeCount := map[string]int{}
	for _, tc := range tools {
		typeCount[tc.toolName]++
	}
	if typeCount["Grep"] != 1 {
		t.Errorf("expected 1 Grep tool, got %d", typeCount["Grep"])
	}
	if typeCount["Read"] != 4 {
		t.Errorf("expected 4 Read tools, got %d", typeCount["Read"])
	}
	if typeCount["Edit"] != 2 {
		t.Errorf("expected 2 Edit tools, got %d", typeCount["Edit"])
	}
	if typeCount["Bash"] != 3 {
		t.Errorf("expected 3 Bash tools, got %d", typeCount["Bash"])
	}

	t.Logf("verified %d tools: %v", len(tools), typeCount)
}

// TestToolOutputTextExtraction tests the various content formats that
// toolOutputText() in index.html must handle. This documents the contract
// between acp-multiplex messages and the JS rendering code.
func TestToolOutputTextExtraction(t *testing.T) {
	// These are the content formats we've seen in the wild.
	// The JS toolOutputText() function must handle all of them.
	cases := []struct {
		name    string
		content json.RawMessage
		want    string
	}{
		{
			name:    "object with text field",
			content: json.RawMessage(`{"type":"text","text":"ok src/auth 0.5s"}`),
			want:    "ok src/auth 0.5s",
		},
		{
			name:    "plain string",
			content: json.RawMessage(`"Build successful"`),
			want:    "Build successful",
		},
		{
			name:    "array of content objects",
			content: json.RawMessage(`[{"type":"text","text":"line 1"},{"type":"text","text":"line 2"}]`),
			want:    "line 1",
		},
		{
			name:    "nested content wrapper",
			content: json.RawMessage(`[{"content":{"type":"text","text":"nested text"}}]`),
			want:    "nested text",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate the JS toolOutputText logic in Go
			got := extractToolOutputText(tc.content)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// extractToolOutputText mirrors the JS toolOutputText() function.
// Used to verify the extraction logic matches what the browser does.
func extractToolOutputText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	// Try as array
	var arr []json.RawMessage
	if json.Unmarshal(content, &arr) == nil && len(arr) > 0 {
		for _, item := range arr {
			// Try item.content.text (nested wrapper)
			var wrapper struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(item, &wrapper) == nil && wrapper.Content.Text != "" {
				return wrapper.Content.Text
			}
			// Try item.text directly
			var obj struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(item, &obj) == nil && obj.Text != "" {
				return obj.Text
			}
		}
	}

	// Try as string
	var s string
	if json.Unmarshal(content, &s) == nil && s != "" {
		return s
	}

	// Try as object with .text
	var obj struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &obj) == nil && obj.Text != "" {
		return obj.Text
	}

	return ""
}

// TestPermissionReplayInteractive verifies that a pending permission
// at the end of replay would be found by the backward search in flushReplay.
// This is a regression test for the fix in commit 13d8e95.
func TestPermissionReplayInteractive(t *testing.T) {
	t.Skip("Integration test: run in a separate VM to avoid disrupting work. Use: go test -run TestPermissionReplayInteractive -count=1 -v")

	tmpDir := t.TempDir()
	sockDir := filepath.Join(tmpDir, "acp-multiplex")
	os.MkdirAll(sockDir, 0700)
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	sid := "test-perm-session"
	msg := func(v interface{}) []byte {
		b, _ := json.Marshal(v)
		return b
	}
	update := func(kind string, extra map[string]interface{}) []byte {
		u := map[string]interface{}{"sessionUpdate": kind}
		for k, v := range extra {
			u[k] = v
		}
		return msg(map[string]interface{}{
			"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]interface{}{"sessionId": sid, "update": u},
		})
	}

	var messages [][]byte
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "method": "acp-multiplex/meta",
		"params": map[string]interface{}{"name": "TEST: Permission"},
	}))
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(`0`),
		"result": map[string]interface{}{
			"protocolVersion": 1,
			"agentInfo":       map[string]string{"name": "test", "title": "Test"},
		},
	}))
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(`0`),
		"result": map[string]interface{}{"sessionId": sid},
	}))

	// Some turns before the permission
	messages = append(messages, update("user_message_chunk", map[string]interface{}{
		"content": map[string]string{"type": "text", "text": "Do something"},
	}))
	messages = append(messages, update("agent_message_chunk", map[string]interface{}{
		"content": map[string]string{"type": "text", "text": "I need permission to proceed."},
	}))
	messages = append(messages, update("turn_complete", nil))

	// Permission request (simulating ExitPlanMode or tool permission)
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(`42`),
		"method": "session/request_permission",
		"params": map[string]interface{}{
			"sessionId": sid,
			"permission": map[string]interface{}{
				"type":    "plan",
				"title":   "Ready to code?",
				"message": "I have a plan to implement the feature.",
			},
		},
	}))

	// Start fake socket + server
	sockPath := filepath.Join(sockDir, "88888.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		for _, m := range messages {
			conn.Write(m)
			conn.Write([]byte("\n"))
		}
		buf := make([]byte, 4096)
		for {
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := conn.Read(buf); err != nil {
				conn.Close()
				return
			}
		}
	}()

	testMode = true
	mux := http.NewServeMux()
	mux.Handle("/ws", &websocket.Server{
		Handler:   func(ws *websocket.Conn) { bridgeWebSocket(ws, sockPath) },
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/ws?sock=88888"
	ws, err := websocket.Dial(wsURL, "", srv.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	var received []string
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var msg string
		if err := websocket.Message.Receive(ws, &msg); err != nil {
			break
		}
		received = append(received, msg)
	}

	// The permission message should be present in the replay
	foundPermission := false
	permIdx := -1
	for i, raw := range received {
		if strings.Contains(raw, "request_permission") {
			foundPermission = true
			permIdx = i
		}
	}

	if !foundPermission {
		t.Fatal("permission message not found in replay")
	}

	// Permission should be the last message (or near-last)
	t.Logf("permission at index %d of %d messages", permIdx, len(received))
	if permIdx < len(received)-2 {
		t.Errorf("permission at index %d but expected near end (total %d)", permIdx, len(received))
		for i := permIdx; i < len(received); i++ {
			t.Logf("  msg[%d]: %.100s", i, received[i])
		}
	}

	_ = fmt.Sprintf // avoid unused import
}

// A resumed convo (session/load or session/resume) never replays a
// session/new response, so the id only appears on session/update
// notifications.  The probe must still recover it — labels.json and
// status.json are keyed by session id.
func TestProbeSocketSessionIDFromResumedReplay(t *testing.T) {
	// Short dir: macOS caps unix socket paths at 104 bytes; t.TempDir() overflows.
	tmpDir, err := os.MkdirTemp("/tmp", "probe")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	sockPath := filepath.Join(tmpDir, "77777.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	messages := [][]byte{
		[]byte(`{"jsonrpc":"2.0","method":"acp-multiplex/meta","params":{"name":"Claude Agent @ home-lab"}}`),
		[]byte(`{"id":0,"jsonrpc":"2.0","result":{"protocolVersion":1,"agentInfo":{"name":"claude-agent-acp","title":"Claude Agent"}}}`),
		[]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"resumed-abc-123","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hello"}}}}`),
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		for _, m := range messages {
			conn.Write(m)
			conn.Write([]byte("\n"))
		}
		buf := make([]byte, 4096)
		for {
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()

	info := probeSocket(sockPath, 77777)
	if info.SessionID != "resumed-abc-123" {
		t.Fatalf("SessionID = %q, want %q (from session/update params)", info.SessionID, "resumed-abc-123")
	}
	if info.BufferName != "Claude Agent @ home-lab" {
		t.Fatalf("BufferName = %q", info.BufferName)
	}
}

func TestMergeTranscriptLabelsPrefersLiveSidecar(t *testing.T) {
	transcripts := []transcriptInfo{
		{SessionID: "session-1", Label: "Durable label"},
		{SessionID: "session-2", Label: "Archive only"},
		{SessionID: "session-3", Label: "Cleared durable label"},
	}

	got := mergeTranscriptLabels(transcripts, map[string]string{
		"session-1": "Fresh label",
		"session-3": "",
	})

	if got[0].Label != "Fresh label" {
		t.Fatalf("live label = %q, want Fresh label", got[0].Label)
	}
	if got[1].Label != "Archive only" {
		t.Fatalf("durable fallback = %q, want Archive only", got[1].Label)
	}
	if got[2].Label != "" {
		t.Fatalf("cleared label = %q, want empty live tombstone", got[2].Label)
	}
}

func TestSearchTranscriptRecordsRanksLabelsBeforeOtherMetadata(t *testing.T) {
	file := makeEligibleTranscriptFile(t, "ranking.md", "no body match\n")
	transcripts := []transcriptInfo{
		{File: file, SessionID: "preview", Preview: "Orbit migration", Timestamp: "2026-09-01-12-00-04"},
		{File: file, SessionID: "agent", Agent: "Orbit", Timestamp: "2026-09-01-12-00-03"},
		{File: file, SessionID: "label", Label: "Orbit control", Timestamp: "2026-08-01-12-00-00"},
		{File: file, SessionID: "project", Project: "orbit", Timestamp: "2026-09-01-12-00-02"},
	}

	results, truncated, err := searchTranscriptRecords(context.Background(), "ORBIT", transcripts)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("four matches should not be truncated")
	}
	wantSessions := []string{"label", "preview", "project", "agent"}
	wantFields := []string{"label", "preview", "project", "agent"}
	if len(results) != len(wantSessions) {
		t.Fatalf("got %d results, want %d", len(results), len(wantSessions))
	}
	for i := range wantSessions {
		if results[i].SessionID != wantSessions[i] || results[i].MatchField != wantFields[i] {
			t.Fatalf("result %d = session %q field %q, want session %q field %q",
				i, results[i].SessionID, results[i].MatchField, wantSessions[i], wantFields[i])
		}
	}
}

func TestSearchTranscriptRecordsFindsLiteralBodyMatchesOnlyInIndexedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "project", ".agent-shell", "transcripts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	literalFile := filepath.Join(dir, "literal.md")
	regexLikeFile := filepath.Join(dir, "regex-like.md")
	unindexedFile := filepath.Join(dir, "unindexed.md")
	if err := os.WriteFile(literalFile, []byte("header\n---\n\n## User (2026-09-01 12:00)\nVersion a.b shipped\nA.B follow-up\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(regexLikeFile, []byte("header\n---\n\n## User (2026-09-01 12:00)\naxb is not a literal match\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unindexedFile, []byte("header\n---\n\n## User (2026-09-01 12:00)\na.b must stay outside results\n"), 0600); err != nil {
		t.Fatal(err)
	}
	transcripts := []transcriptInfo{
		{File: literalFile, SessionID: "literal", Timestamp: "2026-09-01-12-00-00"},
		{File: regexLikeFile, SessionID: "regex-like", Timestamp: "2026-09-01-12-00-01"},
	}

	results, truncated, err := searchTranscriptRecords(context.Background(), "a.b", transcripts)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("one body result should not be truncated")
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want one indexed literal match: %#v", len(results), results)
	}
	got := results[0]
	if got.SessionID != "literal" || got.MatchField != "body" {
		t.Fatalf("match = session %q field %q, want literal/body", got.SessionID, got.MatchField)
	}
	if got.MatchCount != 2 {
		t.Fatalf("matchCount = %d, want 2 case-insensitive literal matches", got.MatchCount)
	}
	if !strings.Contains(got.Snippet, "Version a.b shipped") {
		t.Fatalf("snippet = %q, want first matching line", got.Snippet)
	}
}

func TestSearchTranscriptRecordsIgnoresHeaderAndThoughtOnlyMatches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "project", ".agent-shell", "transcripts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"header":   "Header orbit only\n---\n\n## User (2026-09-01 12:00)\nordinary text\n",
		"thoughts": "Header\n---\n\n## Agent's Thoughts (2026-09-01 12:00)\nprivate orbit thought\n\n## Agent (2026-09-01 12:01)\nordinary answer\n",
		"visible":  "Header\n---\n\n## User (2026-09-01 12:00)\nvisible orbit request\n",
	}
	transcripts := make([]transcriptInfo, 0, len(files))
	for sessionID, content := range files {
		file := filepath.Join(dir, sessionID+".md")
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		transcripts = append(transcripts, transcriptInfo{File: file, SessionID: sessionID})
	}

	results, _, err := searchTranscriptRecords(context.Background(), "orbit", transcripts)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SessionID != "visible" || results[0].MatchLine != 5 {
		t.Fatalf("results = %#v, want only the renderable line-5 match", results)
	}
}

func TestSearchTranscriptRecordsCentersSnippetOnLateColumnMatch(t *testing.T) {
	content := "Header\n---\n\n## User (2026-09-01 12:00)\n" + strings.Repeat("prefix ", 80) + "orbit decision\n"
	file := makeEligibleTranscriptFile(t, "late-column.md", content)

	results, _, err := searchTranscriptRecords(context.Background(), "orbit", []transcriptInfo{{File: file, SessionID: "late"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !strings.Contains(results[0].Snippet, "orbit decision") {
		t.Fatalf("results = %#v, want snippet centered around late-column match", results)
	}
}

func TestSearchTranscriptRecordsSearchesLongLineWithinFileCap(t *testing.T) {
	content := "Header\n---\n\n## User (2026-09-01 12:00)\n" +
		strings.Repeat("x", 300*1024) + "orbit\nvisible orbit fallback\n"
	file := makeEligibleTranscriptFile(t, "long-line.md", content)

	results, _, err := searchTranscriptRecords(context.Background(), "orbit", []transcriptInfo{{File: file, SessionID: "bounded"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].MatchCount != 2 || !strings.Contains(results[0].Snippet, "orbit") {
		t.Fatalf("results = %#v, want both matches and a centered long-line snippet", results)
	}
}

func TestTranscriptSearchHandlerReturnsLiveLabelResult(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".acp-mobile"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(home, ".acp-mobile", "labels.json"),
		[]byte(`{"session-1":"Fresh Orbit label"}`), 0600); err != nil {
		t.Fatal(err)
	}
	transcriptDir := filepath.Join(home, "project", ".agent-shell", "transcripts")
	if err := os.MkdirAll(transcriptDir, 0700); err != nil {
		t.Fatal(err)
	}
	transcriptFile := filepath.Join(transcriptDir, "one.md")
	if err := os.WriteFile(transcriptFile, []byte("nothing in the body\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loader := func(_ context.Context, _ int) ([]transcriptInfo, error) {
		return []transcriptInfo{{
			File: transcriptFile, SessionID: "session-1", Label: "Old label",
			Project: "syzygy", Agent: "Codex", Timestamp: "2026-09-01-12-00-00",
		}}, nil
	}
	body := strings.NewReader(`{"query":"orbit"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/transcript-search", body)
	w := httptest.NewRecorder()

	newTranscriptSearchHandler(loader).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var payload struct {
		Query     string                   `json:"query"`
		Results   []transcriptSearchResult `json:"results"`
		Truncated bool                     `json:"truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Query != "orbit" || payload.Truncated || len(payload.Results) != 1 {
		t.Fatalf("unexpected response: %#v", payload)
	}
	result := payload.Results[0]
	if result.Label != "Fresh Orbit label" || result.MatchField != "label" {
		t.Fatalf("result label/field = %q/%q, want live label/label", result.Label, result.MatchField)
	}
}

func TestSearchTranscriptRecordsCapsGlobalResults(t *testing.T) {
	file := makeEligibleTranscriptFile(t, "cap.md", "no body match\n")
	transcripts := make([]transcriptInfo, 41)
	for i := range transcripts {
		transcripts[i] = transcriptInfo{
			File:      file,
			SessionID: fmt.Sprintf("session-%02d", i),
			Label:     "shared needle",
			Timestamp: fmt.Sprintf("2026-09-01-12-00-%02d", i),
		}
	}

	results, truncated, err := searchTranscriptRecords(context.Background(), "needle", transcripts)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("41 matching conversations should report truncation")
	}
	if len(results) != 40 {
		t.Fatalf("got %d results, want global cap of 40", len(results))
	}
	if results[0].SessionID != "session-40" {
		t.Fatalf("first session = %q, want newest session-40", results[0].SessionID)
	}
}

func TestSearchTranscriptRecordsRejectsIneligibleMetadataMatches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	transcriptDir := filepath.Join(home, "project", ".agent-shell", "transcripts")
	if err := os.MkdirAll(transcriptDir, 0700); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(transcriptDir, "valid.md")
	if err := os.WriteFile(valid, []byte("ordinary transcript\n"), 0600); err != nil {
		t.Fatal(err)
	}
	extraFormat := filepath.Join(home, "project", "notes.md")
	if err := os.WriteFile(extraFormat, []byte("ordinary notes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(transcriptDir, "oversized.md")
	if err := os.WriteFile(oversized, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(oversized, 8*1024*1024+1); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "outside.md")
	if err := os.WriteFile(outside, []byte("outside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(transcriptDir, "symlink.md")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	transcripts := []transcriptInfo{
		{File: valid, SessionID: "valid", Label: "orbit"},
		{File: extraFormat, SessionID: "extra-format", Label: "orbit"},
		{File: oversized, SessionID: "oversized", Label: "orbit"},
		{File: symlink, SessionID: "symlink", Label: "orbit"},
	}

	results, _, err := searchTranscriptRecords(context.Background(), "orbit", transcripts)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SessionID != "valid" {
		t.Fatalf("results = %#v, want only the eligible indexed transcript", results)
	}
}

func makeEligibleTranscriptFile(t *testing.T, name, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "project", ".agent-shell", "transcripts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestTranscriptSearchHandlerRejectsQueriesOutsideBounds(t *testing.T) {
	loader := func(_ context.Context, _ int) ([]transcriptInfo, error) { return []transcriptInfo{}, nil }
	for name, query := range map[string]string{
		"one character":     "x",
		"too long":          strings.Repeat("界", 201),
		"control character": "two\nlines",
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.NewReader(`{"query":` + fmt.Sprintf("%q", query) + `}`)
			req := httptest.NewRequest(http.MethodPost, "/api/transcript-search", body)
			w := httptest.NewRecorder()

			newTranscriptSearchHandler(loader).ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestTranscriptSearchTimeoutIncludesIndexLoad(t *testing.T) {
	loader := func(ctx context.Context, _ int) ([]transcriptInfo, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/transcript-search", strings.NewReader(`{"query":"orbit"}`)).WithContext(ctx)
	w := httptest.NewRecorder()

	newTranscriptSearchHandler(loader).ServeHTTP(w, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 for index timeout: %s", w.Code, w.Body.String())
	}
}

func TestDecodeTranscriptIndexOutputPreservesDurableMetadata(t *testing.T) {
	raw := `[{"file":"/tmp/one.md","project":"syzygy","timestamp":"2026-09-01-12-00-00","agent":"Codex","preview":"Search it","sessionId":"session-1","label":"Recall UX"}]`
	armored := `"` + base64.StdEncoding.EncodeToString([]byte(raw)) + `"`

	got, err := decodeTranscriptIndexOutput([]byte(armored))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d transcripts, want 1", len(got))
	}
	if got[0].SessionID != "session-1" || got[0].Label != "Recall UX" {
		t.Fatalf("decoded transcript = %#v", got[0])
	}
}

func TestTranscriptsHandlerUsesSameLiveLabelAsSearch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".acp-mobile"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(home, ".acp-mobile", "labels.json"),
		[]byte(`{"session-1":"Fresh label"}`), 0600); err != nil {
		t.Fatal(err)
	}
	transcriptDir := filepath.Join(home, "project", ".agent-shell", "transcripts")
	if err := os.MkdirAll(transcriptDir, 0700); err != nil {
		t.Fatal(err)
	}
	transcriptFile := filepath.Join(transcriptDir, "one.md")
	if err := os.WriteFile(transcriptFile, []byte("transcript\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loader := func(_ context.Context, limit int) ([]transcriptInfo, error) {
		if limit != 100 {
			t.Fatalf("limit = %d, want 100", limit)
		}
		return []transcriptInfo{{File: transcriptFile, SessionID: "session-1", Label: "Durable label"}}, nil
	}
	req := httptest.NewRequest(http.MethodPost, "/api/transcripts", nil)
	w := httptest.NewRecorder()

	newTranscriptsHandler(loader).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got []transcriptInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Label != "Fresh label" {
		t.Fatalf("recent transcripts = %#v, want live label", got)
	}
}

func TestTranscriptHandlerRequiresIndexedEligibleFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "project", ".agent-shell", "transcripts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	indexed := filepath.Join(dir, "indexed.md")
	unindexed := filepath.Join(dir, "unindexed.md")
	outside := filepath.Join(home, "outside.md")
	for file, content := range map[string]string{
		indexed: "indexed content", unindexed: "unindexed content", outside: "outside content",
	} {
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	symlink := filepath.Join(dir, "escape.md")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	loader := func(_ context.Context, limit int) ([]transcriptInfo, error) {
		if limit != 0 {
			t.Fatalf("limit = %d, want full index", limit)
		}
		return []transcriptInfo{{File: indexed}, {File: symlink}}, nil
	}
	handler := newTranscriptHandler(loader)
	canonicalIndexed, err := filepath.EvalSymlinks(indexed)
	if err != nil {
		t.Fatal(err)
	}

	for name, test := range map[string]struct {
		file       string
		wantStatus int
	}{
		"indexed":           {indexed, http.StatusOK},
		"indexed canonical": {canonicalIndexed, http.StatusOK},
		"unindexed":         {unindexed, http.StatusForbidden},
		"symlink":           {symlink, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/transcript?file="+url.QueryEscape(test.file), nil)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			if w.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, test.wantStatus, w.Body.String())
			}
			if strings.HasPrefix(name, "indexed") && !strings.Contains(w.Body.String(), "indexed content") {
				t.Fatalf("body = %q, want indexed transcript content", w.Body.String())
			}
		})
	}
}

func TestHistoryDockOwnsSearchAndNewChatOutsideBothScreens(t *testing.T) {
	doc, err := html.Parse(bytes.NewReader(indexHTML))
	if err != nil {
		t.Fatal(err)
	}
	header := htmlNodeByID(doc, "nav-header")
	dock := htmlNodeByID(doc, "history-dock")
	orrery := htmlNodeByID(doc, "orrery")
	history := htmlNodeByID(doc, "history")
	if header == nil || dock == nil || orrery == nil || history == nil {
		t.Fatal("orrery, history, header, and shared dock must all exist")
	}
	for _, id := range []string{"nav-history-btn", "nav-pins-btn"} {
		if !htmlNodeContainsID(header, id) {
			t.Fatalf("header is missing %s", id)
		}
	}
	if htmlNodeContainsID(header, "spawn-btn") {
		t.Fatal("new chat must move out of the top header")
	}
	for _, id := range []string{"history-search-form", "history-search-input", "spawn-btn"} {
		if !htmlNodeContainsID(dock, id) {
			t.Fatalf("shared dock is missing %s", id)
		}
	}
	if htmlNodeContainsID(orrery, "history-dock") || htmlNodeContainsID(history, "history-dock") {
		t.Fatal("dock must be a shared sibling, not owned by Sessions or History")
	}
	spawn := htmlNodeByID(dock, "spawn-btn")
	if spawn == nil || htmlAttr(spawn, "aria-label") == "" || htmlText(spawn) != "" {
		t.Fatal("new chat control must be icon-only with an accessible label")
	}
}

func htmlNodeByID(root *html.Node, id string) *html.Node {
	if root.Type == html.ElementNode && htmlAttr(root, "id") == id {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := htmlNodeByID(child, id); found != nil {
			return found
		}
	}
	return nil
}

func htmlNodeContainsID(root *html.Node, id string) bool {
	return htmlNodeByID(root, id) != nil
}

func htmlAttr(node *html.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

func htmlText(root *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return strings.TrimSpace(b.String())
}

func TestSpawnArgsKeepsPlainShapeAndAppendsCloneOf(t *testing.T) {
	cases := []struct {
		name string
		req  spawnRequest
		want []string
	}{
		{"plain", spawnRequest{Name: "n", Cwd: "/p"}, []string{"n", "/p"}},
		{"task", spawnRequest{Name: "n", Cwd: "/p", Task: "do"}, []string{"n", "/p", "do"}},
		{"preset", spawnRequest{Cwd: "/p", Preset: "f"}, []string{"", "/p", "", "f"}},
		{"clone", spawnRequest{CloneOf: "*agent-shell: x*"}, []string{"", "", "", "", "*agent-shell: x*"}},
		{"clone+name", spawnRequest{Name: "again", CloneOf: "buf"}, []string{"again", "", "", "", "buf"}},
	}
	for _, c := range cases {
		got := spawnArgs(c.req)
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Fatalf("%s: spawnArgs = %q, want %q", c.name, got, c.want)
		}
	}
}

// A clone names a buffer instead of a directory, so cwd must not be
// required; the bridge itself is absent in tests, which surfaces as 500.
func TestHandleSpawnAcceptsCloneOfWithoutCwd(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/spawn", strings.NewReader(body))
		w := httptest.NewRecorder()
		handleSpawn(w, req)
		return w
	}
	if w := post(`{"name":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("no cwd, no cloneOf: status = %d, want 400", w.Code)
	}
	if w := post(`{"cloneOf":"a\nb"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("multi-line cloneOf: status = %d, want 400", w.Code)
	}
	w := post(`{"cloneOf":"*agent-shell: home-lab*"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("cloneOf without cwd: status = %d, want 500 (bridge missing), body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "agent-shell-spawn") {
		t.Fatalf("error should name the bridge, got %s", w.Body.String())
	}
}
