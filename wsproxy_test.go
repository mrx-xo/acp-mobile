package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func wsURL(serverURL, path string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + path
}

func openHub(t *testing.T, h func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	hub := httptest.NewServer(websocket.Server{Handler: h,
		Handshake: func(*websocket.Config, *http.Request) error { return nil }})
	t.Cleanup(hub.Close)
	return hub
}

func TestProxyWebSocketPipesBothWays(t *testing.T) {
	node := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		var msg string
		websocket.Message.Receive(ws, &msg)
		websocket.Message.Send(ws, "echo:"+msg)
	}))
	defer node.Close()
	hub := openHub(t, func(ws *websocket.Conn) {
		proxyWebSocket(ws, wsURL(node.URL, "/ws?sock=1"), "http://hub.test")
	})
	c, err := websocket.Dial(wsURL(hub.URL, "/ws"), "", "http://phone.test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	websocket.Message.Send(c, "hi")
	var got string
	if err := websocket.Message.Receive(c, &got); err != nil {
		t.Fatal(err)
	}
	if got != "echo:hi" {
		t.Fatalf("got %q", got)
	}
}

func TestProxyWebSocketClosesBothSidesOnPeerClose(t *testing.T) {
	node := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {}))
	defer node.Close()
	done := make(chan struct{})
	hub := openHub(t, func(ws *websocket.Conn) {
		proxyWebSocket(ws, wsURL(node.URL, "/ws?sock=1"), "http://hub.test")
		close(done)
	})
	c, err := websocket.Dial(wsURL(hub.URL, "/ws"), "", "http://phone.test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res := make(chan error, 1)
	go func() {
		var s string
		res <- websocket.Message.Receive(c, &s)
	}()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("expected receive error after peer close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("phone side stayed open after peer closed")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxyWebSocket did not return")
	}
}

func TestProxyWebSocketSendsPeerHeader(t *testing.T) {
	got := make(chan string, 1)
	node := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		got <- ws.Request().Header.Get(peerHeader) + "|" + ws.Request().Header.Get("Origin")
	}))
	defer node.Close()
	hub := openHub(t, func(ws *websocket.Conn) {
		proxyWebSocket(ws, wsURL(node.URL, "/ws?sock=1"), "http://hub.test")
	})
	c, err := websocket.Dial(wsURL(hub.URL, "/ws"), "", "http://phone.test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case v := <-got:
		if v != "1|http://hub.test" {
			t.Fatalf("peer header|origin = %q, want 1|http://hub.test", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("node never saw the dial")
	}
}

func TestProxyWebSocketPeerUnreachable(t *testing.T) {
	node := httptest.NewServer(http.NotFoundHandler())
	dead := wsURL(node.URL, "/ws?sock=1")
	node.Close()
	hub := openHub(t, func(ws *websocket.Conn) {
		if err := proxyWebSocket(ws, dead, "http://hub.test"); err == nil {
			t.Error("expected dial error")
		}
	})
	c, err := websocket.Dial(wsURL(hub.URL, "/ws"), "", "http://phone.test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var s string
	if err := websocket.Message.Receive(c, &s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "peer unreachable") || !strings.Contains(s, "-32000") {
		t.Fatalf("frame = %s", s)
	}
}

func TestWSHandlerRejectsUnknownMachine(t *testing.T) {
	withFleetFixture(t, "mrx")
	old := testMode
	testMode = true
	t.Cleanup(func() { testMode = old })
	srv := httptest.NewServer(&websocket.Server{Handler: handleWS, Handshake: wsHandshake})
	defer srv.Close()
	c, err := websocket.Dial(wsURL(srv.URL, "/ws?machine=ghost&sock=1"), "", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var s string
	if err := websocket.Message.Receive(c, &s); err != nil {
		t.Fatal(err)
	}
	var f struct {
		Error struct {
			Code int `json:"code"`
			Data struct {
				Details string `json:"details"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		t.Fatal(err)
	}
	if f.Error.Code != -32000 || !strings.Contains(f.Error.Data.Details, "unknown machine ghost") {
		t.Fatalf("frame = %s", s)
	}
}

func TestWSHandlerProxiesToKnownMachine(t *testing.T) {
	node := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		websocket.Message.Send(ws, "node:"+ws.Request().URL.Query().Get("sock"))
	}))
	defer node.Close()
	withFleetFixture(t, "mrx", peerAt(t, "castor", "CASTOR", node.URL))
	old := testMode
	testMode = true
	t.Cleanup(func() { testMode = old })
	srv := httptest.NewServer(&websocket.Server{Handler: handleWS, Handshake: wsHandshake})
	defer srv.Close()
	c, err := websocket.Dial(wsURL(srv.URL, "/ws?machine=castor&sock=42"), "", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var s string
	if err := websocket.Message.Receive(c, &s); err != nil {
		t.Fatal(err)
	}
	if s != "node:42" {
		t.Fatalf("got %q", s)
	}
}

func TestProxyWebSocketKeepsLargeMessagesWhole(t *testing.T) {
	big := `{"x":"` + strings.Repeat("a", 200*1024) + `"}`
	nodeGot := make(chan string, 1)
	node := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		var in string
		websocket.Message.Receive(ws, &in)
		nodeGot <- in
		websocket.Message.Send(ws, big)
	}))
	defer node.Close()
	hub := openHub(t, func(ws *websocket.Conn) {
		proxyWebSocket(ws, wsURL(node.URL, "/ws?sock=1"), "http://hub.test")
	})
	c, err := websocket.Dial(wsURL(hub.URL, "/ws"), "", "http://phone.test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	websocket.Message.Send(c, big)
	var out string
	if err := websocket.Message.Receive(c, &out); err != nil {
		t.Fatal(err)
	}
	if out != big {
		t.Fatalf("node->phone: got %d bytes in first message, want %d", len(out), len(big))
	}
	if in := <-nodeGot; in != big {
		t.Fatalf("phone->node: got %d bytes in first message, want %d", len(in), len(big))
	}
}
