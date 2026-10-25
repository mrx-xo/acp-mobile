package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"golang.org/x/net/websocket"
)

// hubOrigin is the Origin the hub presents when dialing a node; set in main
// once the hub's own address is known.
var hubOrigin = "http://127.0.0.1:8090"

// peerWSURL is the node-side websocket URL for a session pid.
func peerWSURL(m machine, pid string) string {
	return "ws://" + peerHostPort(m) + "/ws?sock=" + pid
}

// proxyWebSocket dials target as a fleet peer and pipes frames between the
// phone and the node until either side ends, then closes both.
func proxyWebSocket(phone *websocket.Conn, target, origin string) error {
	config, err := websocket.NewConfig(target, origin)
	if err == nil {
		config.Header = http.Header{peerHeader: {"1"}}
	}
	var node *websocket.Conn
	if err == nil {
		node, err = websocket.DialConfig(config)
	}
	if err != nil {
		websocket.Message.Send(phone, `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Session not found","data":{"details":`+jsonString(fmt.Sprintf("peer unreachable: %v", err))+`}}}`)
		phone.Close()
		return err
	}
	phone.PayloadType = websocket.TextFrame
	node.PayloadType = websocket.TextFrame

	done := make(chan struct{}, 2)
	pipe := func(dst, src *websocket.Conn) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go pipe(node, phone)
	go pipe(phone, node)
	<-done
	phone.Close()
	node.Close()
	<-done
	return nil
}

// handleWS serves /ws: local sessions bridge to their socket, sessions on
// another fleet machine are proxied to that machine's hub.
func handleWS(ws *websocket.Conn) {
	q := ws.Request().URL.Query()
	pid := q.Get("sock")
	machineID := q.Get("machine")
	if machineID != "" {
		fleet.mu.RLock()
		self := fleet.self
		fleet.mu.RUnlock()
		if machineID != self {
			fleet.mu.RLock()
			m, ok := fleet.reg.byID(machineID)
			fleet.mu.RUnlock()
			if !ok {
				log.Printf("ws: unknown machine %q", machineID)
				websocket.Message.Send(ws, `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Session not found","data":{"details":`+jsonString("unknown machine "+machineID)+`}}}`)
				ws.Close()
				return
			}
			if !validPid(ws, pid) {
				return
			}
			log.Printf("ws: proxy start machine=%s pid=%s", machineID, pid)
			err := proxyWebSocket(ws, peerWSURL(m, pid), hubOrigin)
			log.Printf("ws: proxy end machine=%s pid=%s err=%v", machineID, pid, err)
			return
		}
	}
	if !validPid(ws, pid) {
		return
	}
	sockPath := findSocket(pid)
	if sockPath == "" {
		log.Printf("ws: no socket for pid %s", pid)
		websocket.Message.Send(ws, `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Session not found","data":{"details":"No socket for pid `+pid+`"}}}`)
		ws.Close()
		return
	}
	bridgeWebSocket(ws, sockPath)
}

// validPid applies the same sock checks as the local path; it closes ws and
// returns false when the param is missing or non-numeric.
func validPid(ws *websocket.Conn, pid string) bool {
	if pid == "" {
		log.Printf("ws: missing sock param")
		ws.Close()
		return false
	}
	if _, err := strconv.Atoi(pid); err != nil {
		log.Printf("ws: invalid sock param %q", pid)
		ws.Close()
		return false
	}
	return true
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
