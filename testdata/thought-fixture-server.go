// Command thought-fixture-server exposes thought-replay.jsonl through the
// Unix-socket interface used by acp-multiplex. It sends the first four records
// as replay, streams the remaining records to the first durable connection,
// and serves the completed history as replay on later connections.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type fixtureServer struct {
	mu        sync.Mutex
	messages  []string
	prefix    int
	streaming bool
	completed bool
}

func main() {
	fixturePath := flag.String("fixture", "testdata/thought-replay.jsonl", "JSONL fixture path")
	replayDelay := flag.Duration("replay-delay", 350*time.Millisecond, "pause before live delivery")
	chunkDelay := flag.Duration("chunk-delay", 180*time.Millisecond, "pause between live records")
	flag.Parse()

	messages, err := readFixture(*fixturePath)
	if err != nil {
		fatalf("read fixture: %v", err)
	}
	const replayPrefix = 4
	if len(messages) <= replayPrefix {
		fatalf("fixture has %d records; need more than %d", len(messages), replayPrefix)
	}

	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = os.TempDir()
	}
	socketDir := filepath.Join(runtimeDir, "acp-multiplex")
	if err := os.MkdirAll(socketDir, 0700); err != nil {
		fatalf("create socket directory: %v", err)
	}
	socketPath := filepath.Join(socketDir, fmt.Sprintf("%d.sock", os.Getpid()))
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		fatalf("remove stale socket: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fatalf("listen: %v", err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	server := &fixtureServer{messages: messages, prefix: replayPrefix}
	fmt.Printf("thought fixture ready: pid=%d socket=%s\n", os.Getpid(), socketPath)
	fmt.Println("start acp-mobile with: go run . --test-mode 18091")
	fmt.Println("then open its authenticated URL and select TEST: Thought Rendering")

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			continue
		}
		go server.serve(conn, *replayDelay, *chunkDelay)
	}
}

func readFixture(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var messages []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			messages = append(messages, line)
		}
	}
	return messages, scanner.Err()
}

func (s *fixtureServer) serve(conn net.Conn, replayDelay, chunkDelay time.Duration) {
	defer conn.Close()

	s.mu.Lock()
	completed := s.completed
	s.mu.Unlock()
	if completed {
		if sendMessages(conn, s.messages, 0) {
			_, _ = io.Copy(io.Discard, conn)
		}
		return
	}

	if !sendMessages(conn, s.messages[:s.prefix], 0) {
		return
	}
	time.Sleep(replayDelay)

	// Session discovery probes disconnect after the prefix. Only one durable
	// bridge connection gets to claim live delivery; contenders wait for it.
	for {
		s.mu.Lock()
		if s.completed {
			s.mu.Unlock()
			if sendMessages(conn, s.messages[s.prefix:], 0) {
				_, _ = io.Copy(io.Discard, conn)
			}
			return
		}
		if !s.streaming {
			s.streaming = true
			s.mu.Unlock()
			break
		}
		s.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}

	ok := sendMessages(conn, s.messages[s.prefix:], chunkDelay)
	s.mu.Lock()
	s.streaming = false
	if ok {
		s.completed = true
	}
	s.mu.Unlock()
	if ok {
		fmt.Println("live sequence complete; new connections now receive full replay")
		_, _ = io.Copy(io.Discard, conn)
	}
}

func sendMessages(conn net.Conn, messages []string, delay time.Duration) bool {
	for index, message := range messages {
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := io.WriteString(conn, message+"\n"); err != nil {
			return false
		}
		if delay > 0 && index < len(messages)-1 {
			time.Sleep(delay)
		}
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return true
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
