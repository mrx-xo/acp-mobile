package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/net/websocket"
)

//go:embed index.html
var indexHTML []byte

// buildID fingerprints the embedded UI; clients compare it against
// /api/sessions' version and reload themselves when it changes — iOS
// standalone web apps resume frozen pages and never refetch on their own.
var buildID = func() string {
	sum := sha256.Sum256(indexHTML)
	return hex.EncodeToString(sum[:4])
}()

// Iosevka Term Slab (subset woff2) — same face the Emacs rig runs, so
// the phone UI and agent-shell read as one tool.
//
//go:embed fonts
var fontsFS embed.FS

var validBufferName = regexp.MustCompile(`^[\w\s.\-@<>/()]+$`)

// --- Configuration ---

type config struct {
	ExtraPath []string `json:"extraPath"`
}

var appConfig config

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".acp-mobile", "config.json")
}

func loadConfig() config {
	var cfg config
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, &cfg)
	return cfg
}

// command creates an exec.Cmd with extra PATH directories from config.
func command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	configureCommandEnv(cmd)
	return cmd
}

func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	configureCommandEnv(cmd)
	return cmd
}

func configureCommandEnv(cmd *exec.Cmd) {
	if len(appConfig.ExtraPath) > 0 {
		env := os.Environ()
		extra := strings.Join(appConfig.ExtraPath, ":")
		found := false
		for i, e := range env {
			if strings.HasPrefix(e, "PATH=") {
				env[i] = "PATH=" + extra + ":" + e[5:]
				found = true
				break
			}
		}
		if !found {
			env = append(env, "PATH="+extra)
		}
		cmd.Env = env
	}
}

type tailscaleInfo struct {
	Hostname string
	IP       string
}

func tailscaleSelf() (tailscaleInfo, error) {
	out, err := command("tailscale", "status", "--self", "--json").Output()
	if err != nil {
		return tailscaleInfo{}, err
	}
	var status struct {
		Self struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return tailscaleInfo{}, err
	}
	name := strings.TrimSuffix(status.Self.DNSName, ".")
	if name == "" {
		return tailscaleInfo{}, fmt.Errorf("no tailscale hostname")
	}
	var ip string
	for _, addr := range status.Self.TailscaleIPs {
		if strings.Contains(addr, ".") {
			ip = addr
			break
		}
	}
	return tailscaleInfo{Hostname: name, IP: ip}, nil
}

func authKeyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".acp-mobile", "authkey")
}

func loadOrCreateAuthKey() string {
	p := authKeyPath()
	data, err := os.ReadFile(p)
	if err == nil {
		key := strings.TrimSpace(string(data))
		if key != "" {
			return key
		}
	}
	// Generate new key
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("failed to generate authkey: %v", err)
	}
	key := hex.EncodeToString(b)
	os.MkdirAll(filepath.Dir(p), 0700)
	if err := os.WriteFile(p, []byte(key+"\n"), 0600); err != nil {
		log.Fatalf("failed to write authkey: %v", err)
	}
	log.Printf("generated new authkey at %s", p)
	return key
}

// Auth rate limiter: tracks failed attempts per IP
type rateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{attempts: make(map[string][]time.Time)}
}

// check returns true if the IP is rate-limited (too many recent failures).
func (rl *rateLimiter) check(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	window := time.Now().Add(-15 * time.Minute)
	attempts := rl.attempts[ip]
	// Prune old entries
	valid := attempts[:0]
	for _, t := range attempts {
		if t.After(window) {
			valid = append(valid, t)
		}
	}
	rl.attempts[ip] = valid
	return len(valid) >= 10 // 10 failures in 15 min = locked out
}

func (rl *rateLimiter) record(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.attempts[ip] = append(rl.attempts[ip], time.Now())
}

func (rl *rateLimiter) reset(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.attempts, ip)
}

var testMode bool

func main() {
	appConfig = loadConfig()

	port := "8090"
	for _, arg := range os.Args[1:] {
		if arg == "--test-mode" {
			testMode = true
		} else {
			port = arg
		}
	}

	authKey := loadOrCreateAuthKey()
	authRL := newRateLimiter()

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// Generate per-request CSP nonce
		nonceBytes := make([]byte, 16)
		rand.Read(nonceBytes)
		nonce := base64.StdEncoding.EncodeToString(nonceBytes)

		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy",
			fmt.Sprintf("default-src 'self'; img-src 'self' data: blob:; script-src 'nonce-%s'; style-src 'unsafe-inline'; frame-ancestors 'none'", nonce))
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := bytes.Replace(indexHTML, []byte("__CSP_NONCE__"), []byte(nonce), 1)
		page = bytes.Replace(page, []byte("__BUILD_ID__"), []byte(buildID), 1)
		w.Write(page)
	})

	mux.Handle("/ws", &websocket.Server{
		Handler: func(ws *websocket.Conn) {
			pid := ws.Request().URL.Query().Get("sock")
			if pid == "" {
				log.Printf("ws: missing sock param")
				ws.Close()
				return
			}
			if _, err := strconv.Atoi(pid); err != nil {
				log.Printf("ws: invalid sock param %q", pid)
				ws.Close()
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
		},
		Handshake: func(config *websocket.Config, r *http.Request) error {
			origin := r.Header.Get("Origin")
			if origin == "" {
				if testMode {
					return nil
				}
				return fmt.Errorf("missing Origin header")
			}
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				return fmt.Errorf("origin %q not allowed", origin)
			}
			config.Origin, _ = websocket.Origin(config, r)
			return nil
		},
	})

	mux.Handle("/fonts/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.FileServerFS(fontsFS).ServeHTTP(w, r)
	}))

	mux.HandleFunc("/apple-touch-icon.png", func(w http.ResponseWriter, r *http.Request) {
		data, err := fontsFS.ReadFile("fonts/apple-touch-icon.png")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
	})

	mux.HandleFunc("/api/sessions", handleSessions)
	mux.HandleFunc("/api/statuses", handleStatuses)
	mux.HandleFunc("/api/spawn", handleSpawn)
	mux.HandleFunc("/api/kill", handleKill)
	mux.HandleFunc("/api/label", handleLabel)
	mux.HandleFunc("/api/preview", handlePreview)
	mux.HandleFunc("/api/transcripts", handleTranscripts)
	mux.HandleFunc("/api/transcript-search", handleTranscriptSearch)
	mux.HandleFunc("/api/transcript", handleTranscript)
	mux.HandleFunc("/files/list", handleFileList)
	mux.HandleFunc("/files/read", handleFileRead)

	// CSRF protection via Sec-Fetch-Site headers
	cop := http.NewCrossOriginProtection()

	// Allowed hosts for DNS rebinding protection
	allowedHosts := map[string]bool{
		"127.0.0.1": true,
		"localhost": true,
	}
	ts, tsErr := tailscaleSelf()
	if tsErr == nil {
		allowedHosts[ts.Hostname] = true
		if ts.IP != "" {
			allowedHosts[ts.IP] = true
		}
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DNS rebinding protection
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !allowedHosts[host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// Extract client IP for rate limiting
		clientIP := r.RemoteAddr
		if h, _, err := net.SplitHostPort(clientIP); err == nil {
			clientIP = h
		}

		// The icon must be public: iOS fetches apple-touch-icon WITHOUT
		// cookies during Add to Home Screen; a 401 silently falls back
		// to a page-screenshot icon. It's just a glyph — no secrets.
		if r.URL.Path == "/apple-touch-icon.png" {
			mux.ServeHTTP(w, r)
			return
		}

		// Auth: if authkey query param is present and valid, set cookie and redirect
		if qk := r.URL.Query().Get("authkey"); qk != "" {
			if subtle.ConstantTimeCompare([]byte(qk), []byte(authKey)) != 1 {
				if authRL.check(clientIP) {
					http.Error(w, "too many failed attempts, try again later", http.StatusTooManyRequests)
					return
				}
				authRL.record(clientIP)
				http.Error(w, "invalid authkey", http.StatusForbidden)
				return
			}
			authRL.reset(clientIP)
			http.SetCookie(w, &http.Cookie{
				Name:     "authkey",
				Value:    authKey,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				// Persistent: iOS home-screen web clips keep their own
				// cookie jar; a session cookie dies every time the clip
				// closes, forcing re-auth the clip can't perform.
				MaxAge: 365 * 24 * 60 * 60,
			})
			// Serve directly — do NOT redirect-strip the authkey from the
			// URL. "Add to Home Screen" snapshots the post-redirect URL,
			// so stripping produced key-less web clips that landed on
			// "unauthorized" once the session cookie was gone. Keeping
			// the key in the URL makes bookmarks/clips self-authenticating
			// (tailnet-only service; the link file already holds the key).
			mux.ServeHTTP(w, r)
			return
		}

		// Auth: check cookie
		cookie, err := r.Cookie("authkey")
		if err != nil {
			// No cookie at all: a stale bookmark/web clip, not an attack.
			// Don't feed the rate limiter — that was locking users out of
			// the GOOD link after a few opens of a key-less icon.
			http.Error(w, "unauthorized — open the full link (with authkey) from ~/.acp-mobile/link or the Telegram message, then re-add to Home Screen", http.StatusUnauthorized)
			return
		}
		if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(authKey)) != 1 {
			if authRL.check(clientIP) {
				http.Error(w, "too many failed attempts, try again later", http.StatusTooManyRequests)
				return
			}
			authRL.record(clientIP)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		mux.ServeHTTP(w, r)
	})

	// Write link file
	link := fmt.Sprintf("http://127.0.0.1:%s?authkey=%s", port, authKey)
	if tsErr == nil {
		link = fmt.Sprintf("http://%s:%s?authkey=%s", ts.Hostname, port, authKey)
	}
	log.Printf("acp-mobile: %s", link)
	linkPath := filepath.Join(filepath.Dir(authKeyPath()), "link")
	os.WriteFile(linkPath, []byte(link+"\n"), 0600)

	wrapped := cop.Handler(handler)

	// Limit request body size (1MB)
	limited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		wrapped.ServeHTTP(w, r)
	})

	// No ReadTimeout/WriteTimeout because they kill long-lived WebSocket
	// connections. ReadHeaderTimeout protects against slowloris without
	// affecting WebSockets (only applies during header read).
	makeServer := func(addr string) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           limited,
			IdleTimeout:       120 * time.Second,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	// Listen on localhost
	go func() {
		log.Fatal(makeServer(fmt.Sprintf("127.0.0.1:%s", port)).ListenAndServe())
	}()
	// Listen on Tailscale IP if available
	if tsErr == nil && ts.IP != "" {
		log.Printf("acp-mobile: also listening on %s:%s", ts.IP, port)
		log.Fatal(makeServer(fmt.Sprintf("%s:%s", ts.IP, port)).ListenAndServe())
	} else {
		select {}
	}
}

// --- Socket discovery ---

func socketDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "acp-multiplex")
}

type socketEntry struct {
	pid   int
	path  string
	mtime int64
}

func discoverSockets() []socketEntry {
	var socks []socketEntry
	seen := map[int]bool{}

	// New location: $TMPDIR/acp-multiplex/<pid>.sock
	dir := socketDir()
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".sock") {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSuffix(name, ".sock"))
			if err != nil {
				continue
			}
			if syscall.Kill(pid, 0) != nil {
				os.Remove(filepath.Join(dir, name))
				continue
			}
			seen[pid] = true
			var mt int64
			if info, err := e.Info(); err == nil {
				mt = info.ModTime().Unix()
			}
			socks = append(socks, socketEntry{pid, filepath.Join(dir, name), mt})
		}
	}

	// Legacy: $TMPDIR/acp-multiplex-<pid>.sock
	tmpdir := os.TempDir()
	if entries, err := os.ReadDir(tmpdir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "acp-multiplex-") || !strings.HasSuffix(name, ".sock") {
				continue
			}
			pidStr := strings.TrimSuffix(strings.TrimPrefix(name, "acp-multiplex-"), ".sock")
			pid, err := strconv.Atoi(pidStr)
			if err != nil || seen[pid] {
				continue
			}
			if syscall.Kill(pid, 0) != nil {
				os.Remove(filepath.Join(tmpdir, name))
				continue
			}
			var mt int64
			if info, err := e.Info(); err == nil {
				mt = info.ModTime().Unix()
			}
			socks = append(socks, socketEntry{pid, filepath.Join(tmpdir, name), mt})
		}
	}

	return socks
}

func findSocket(pidStr string) string {
	p := filepath.Join(socketDir(), pidStr+".sock")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	p = filepath.Join(os.TempDir(), "acp-multiplex-"+pidStr+".sock")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// --- Session probing ---

type sessionInfo struct {
	Pid          int    `json:"pid"`
	SessionID    string `json:"sessionId,omitempty"`
	Title        string `json:"title,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Project      string `json:"project,omitempty"`
	BufferName   string `json:"bufferName,omitempty"`
	Preview      string `json:"preview,omitempty"` // first user message, for card headlines
	Label        string `json:"label,omitempty"`   // user-set label from labels.json sidecar
	Status       string `json:"status,omitempty"`  // busy/permission/idle from status.json sidecar
	LastActivity int64  `json:"lastActivity"`      // unix timestamp
}

// loadSidecar reads a sessionId→value JSON sidecar written by Emacs.
// Missing/corrupt file is not an error — sidecars are optional.
func loadSidecar(name string) map[string]string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".acp-mobile", name))
	if err != nil {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return m
}

// loadLabels reads the sessionId→label sidecar written by Emacs (or by hand).
// Missing/corrupt file is not an error — labels are optional.
func loadLabels() map[string]string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".acp-mobile", "labels.json"))
	if err != nil {
		return nil
	}
	var labels map[string]string
	if json.Unmarshal(data, &labels) != nil {
		return nil
	}
	return labels
}

// currentStatuses returns the latest session status snapshot.  Emacs owns
// rig-driven turn state; phoneTurns fills the one gap for prompts submitted
// through this process.  Keeping this separate from session discovery lets
// the navigator refresh status frequently without probing every socket.
func currentStatuses() map[string]string {
	statuses := loadSidecar("status.json")
	if statuses == nil {
		statuses = make(map[string]string)
	}

	phoneTurns.mu.Lock()
	defer phoneTurns.mu.Unlock()
	for sessionID, count := range phoneTurns.m {
		if count > 0 && statuses[sessionID] != "permission" {
			statuses[sessionID] = "busy"
		}
	}
	return statuses
}

func handleStatuses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"statuses": currentStatuses(),
		"version":  buildID,
	})
}

// probeCache: identity fields from the last successful probe of each pid.
// /api/sessions probes race a 3s budget; under load a slow probe used to
// return a bare {pid} card with no bufferName/sessionId.
var probeCache = struct {
	mu sync.Mutex
	m  map[int]sessionInfo
}{m: map[int]sessionInfo{}}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	socks := discoverSockets()

	// Prune cache entries for sockets that no longer exist.
	alive := map[int]bool{}
	for _, s := range socks {
		alive[s.pid] = true
	}
	probeCache.mu.Lock()
	for pid := range probeCache.m {
		if !alive[pid] {
			delete(probeCache.m, pid)
		}
	}
	probeCache.mu.Unlock()

	type result struct {
		info sessionInfo
		ok   bool
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	results := make([]result, len(socks))

	for i, s := range socks {
		idx := i
		sock := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := make(chan struct{})
			var info sessionInfo
			go func() {
				info = probeSocket(sock.path, sock.pid)
				close(done)
			}()
			select {
			case <-done:
				info.LastActivity = sock.mtime
				if info.BufferName != "" || info.SessionID != "" {
					probeCache.mu.Lock()
					probeCache.m[sock.pid] = info
					probeCache.mu.Unlock()
				}
				results[idx] = result{info: info, ok: true}
			case <-ctx.Done():
				// Timed out — fall back to the last successful probe's
				// identity fields.  A bare {pid} card can't be matched by
				// bufferName/sessionId, which breaks the client's card
				// keying and reconnect re-resolve exactly when a rig-side
				// reload makes them matter most.
				probeCache.mu.Lock()
				cached := probeCache.m[sock.pid]
				probeCache.mu.Unlock()
				cached.Pid = sock.pid
				cached.LastActivity = sock.mtime
				cached.Status = "" // liveness is stale; sidecars re-merge below
				results[idx] = result{info: cached, ok: true}
			}
		}()
	}
	wg.Wait()

	var sessions []sessionInfo
	for _, r := range results {
		if r.ok {
			sessions = append(sessions, r.info)
		}
	}

	// One card per CONVO, not per socket.  The rig's reload (SPC c y)
	// kills the client and respawns it with the SAME buffer name but a
	// NEW pid and — because claude resume forks — a NEW session id.  So
	// the buffer name is the only identity that survives a resync; fall
	// back to session id for non-rig sessions, and let mid-handshake
	// sockets (neither yet) pass through untouched.  Newest socket wins
	// — the dying pid can still pass discovery's liveness check.
	byKey := map[string]int{}
	deduped := sessions[:0]
	for _, s := range sessions {
		key := s.BufferName
		if key == "" {
			key = s.SessionID
		}
		if key == "" {
			deduped = append(deduped, s)
			continue
		}
		if j, ok := byKey[key]; ok {
			if s.LastActivity > deduped[j].LastActivity ||
				(s.LastActivity == deduped[j].LastActivity && s.Pid > deduped[j].Pid) {
				deduped[j] = s
			}
			continue
		}
		byKey[key] = len(deduped)
		deduped = append(deduped, s)
	}
	sessions = deduped

	labels := loadLabels()
	statuses := currentStatuses()
	for i := range sessions {
		if l, ok := labels[sessions[i].SessionID]; ok {
			sessions[i].Label = l
		}
		if st, ok := statuses[sessions[i].SessionID]; ok {
			sessions[i].Status = st
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"sessions": sessions, "version": buildID})
}

func handleSpawn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Cwd    string `json:"cwd"`
		Name   string `json:"name"`
		Task   string `json:"task"`
		Preset string `json:"preset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Cwd == "" {
		http.Error(w, "cwd is required", http.StatusBadRequest)
		return
	}
	// Preset is a single char key into the rig's mr-x/agent-shell-presets;
	// Emacs resolves it (and errors on unknown keys).
	if req.Preset != "" && (len(req.Preset) != 1 || req.Preset[0] < 'a' || req.Preset[0] > 'z') {
		http.Error(w, "invalid preset", http.StatusBadRequest)
		return
	}

	args := []string{req.Name, req.Cwd}
	if req.Task != "" || req.Preset != "" {
		args = append(args, req.Task)
	}
	if req.Preset != "" {
		args = append(args, req.Preset)
	}

	out, err := evalEmacs("agent-shell-spawn", args...)
	if err != nil {
		log.Printf("spawn: %v: %s", err, out)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out)))})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handlePreview returns the tail of a session's conversation — the
// multiplex socket replays full history to any connection, so we dial,
// drain the replay, and keep the last few user/agent messages.
func handlePreview(w http.ResponseWriter, r *http.Request) {
	pid := r.URL.Query().Get("pid")
	if _, err := strconv.Atoi(pid); err != nil {
		http.Error(w, "invalid pid", http.StatusBadRequest)
		return
	}
	sockPath := findSocket(pid)
	if sockPath == "" {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		http.Error(w, "connect failed", http.StatusBadGateway)
		return
	}
	defer conn.Close()

	type pvMsg struct {
		Role string `json:"role"`
		Text string `json:"text"`
	}
	var msgs []pvMsg

	appendMsg := func(role, text string) {
		if text == "" {
			return
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Text += text
			return
		}
		msgs = append(msgs, pvMsg{Role: role, Text: text})
		// Rolling window: coalesced turns, keep a small tail
		if len(msgs) > 12 {
			msgs = msgs[len(msgs)-12:]
		}
	}

	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 256*1024)
	total := 0
	for {
		// Replay has no end marker; a quiet socket means it's drained.
		conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
		n, err := conn.Read(tmp)
		if n > 0 {
			total += n
			buf = append(buf, tmp[:n]...)
			for {
				nl := bytes.IndexByte(buf, '\n')
				if nl < 0 {
					break
				}
				line := buf[:nl]
				buf = buf[nl+1:]
				var msg struct {
					Method string `json:"method"`
					Params struct {
						Update struct {
							SessionUpdate string `json:"sessionUpdate"`
							Content       struct {
								Text string `json:"text"`
							} `json:"content"`
						} `json:"update"`
					} `json:"params"`
				}
				if json.Unmarshal(line, &msg) != nil || msg.Method != "session/update" {
					continue
				}
				switch msg.Params.Update.SessionUpdate {
				case "user_message_chunk":
					appendMsg("user", msg.Params.Update.Content.Text)
				case "agent_message_chunk":
					appendMsg("agent", msg.Params.Update.Content.Text)
				}
			}
		}
		if err != nil || total > 8*1024*1024 {
			break
		}
	}

	// Final trim: last 8 turns, each capped for the sheet
	if len(msgs) > 8 {
		msgs = msgs[len(msgs)-8:]
	}
	for i := range msgs {
		runes := []rune(msgs[i].Text)
		if len(runes) > 700 {
			msgs[i].Text = "…" + string(runes[len(runes)-700:])
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"messages": msgs})
}

// --- Transcript history (syzygy-recall) ---

type transcriptInfo struct {
	File      string `json:"file"`
	Project   string `json:"project"`
	Timestamp string `json:"timestamp"`
	Agent     string `json:"agent"`
	Preview   string `json:"preview"`
	SessionID string `json:"sessionId"`
	Label     string `json:"label,omitempty"`
}

type transcriptSearchResult struct {
	transcriptInfo
	Snippet    string `json:"snippet"`
	MatchField string `json:"matchField"`
	MatchCount int    `json:"matchCount"`
	MatchLine  int    `json:"matchLine,omitempty"`
}

const transcriptSearchLimit = 40

type transcriptBodyMatch struct {
	snippet string
	count   int
	line    int
}

// mergeTranscriptLabels overlays the immediate labels.json sidecar onto
// agent-recall's durable metadata.  The copy keeps cached index responses
// immutable between requests.
func mergeTranscriptLabels(transcripts []transcriptInfo, live map[string]string) []transcriptInfo {
	merged := append([]transcriptInfo(nil), transcripts...)
	for i := range merged {
		if label, ok := live[merged[i].SessionID]; ok {
			merged[i].Label = label
		}
	}
	return merged
}

func searchTranscriptRecords(ctx context.Context, query string, transcripts []transcriptInfo) ([]transcriptSearchResult, bool, error) {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return []transcriptSearchResult{}, false, nil
	}
	transcripts, err := eligibleTranscriptRecords(transcripts)
	if err != nil {
		return nil, false, err
	}
	bodyMatches, err := searchTranscriptBodies(ctx, query, transcripts)
	if err != nil {
		return nil, false, err
	}
	type rankedResult struct {
		transcriptSearchResult
		rank int
	}
	ranked := make([]rankedResult, 0)
	for _, transcript := range transcripts {
		fields := []struct {
			name  string
			value string
			rank  int
		}{
			{"label", transcript.Label, 0},
			{"preview", transcript.Preview, 1},
			{"project", transcript.Project, 2},
			{"agent", transcript.Agent, 3},
		}
		bestRank := len(fields) + 1
		bestField := ""
		bestSnippet := ""
		matches := 0
		for _, field := range fields {
			count := strings.Count(strings.ToLower(field.value), needle)
			if count == 0 {
				continue
			}
			matches += count
			if field.rank < bestRank {
				bestRank = field.rank
				bestField = field.name
				bestSnippet = field.value
			}
		}
		body, bodyMatched := bodyMatches[filepath.Clean(transcript.File)]
		if bodyMatched {
			matches += body.count
			if bestField == "" {
				bestRank = 4
				bestField = "body"
				bestSnippet = body.snippet
			}
		}
		if bestField == "" {
			continue
		}
		ranked = append(ranked, rankedResult{
			transcriptSearchResult: transcriptSearchResult{
				transcriptInfo: transcript,
				Snippet:        bestSnippet,
				MatchField:     bestField,
				MatchCount:     matches,
				MatchLine:      body.line,
			},
			rank: bestRank,
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].rank != ranked[j].rank {
			return ranked[i].rank < ranked[j].rank
		}
		if ranked[i].Timestamp != ranked[j].Timestamp {
			return ranked[i].Timestamp > ranked[j].Timestamp
		}
		return ranked[i].SessionID < ranked[j].SessionID
	})
	truncated := len(ranked) > transcriptSearchLimit
	if truncated {
		ranked = ranked[:transcriptSearchLimit]
	}
	results := make([]transcriptSearchResult, len(ranked))
	for i := range ranked {
		results[i] = ranked[i].transcriptSearchResult
	}
	return results, truncated, nil
}

func searchTranscriptBodies(ctx context.Context, query string, transcripts []transcriptInfo) (map[string]transcriptBodyMatch, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool)
	dirSet := make(map[string]bool)
	for _, transcript := range transcripts {
		clean := filepath.Clean(transcript.File)
		if !allowedTranscriptPath(clean, home) {
			continue
		}
		info, err := os.Stat(clean)
		if err != nil || info.Size() > 8*1024*1024 {
			continue
		}
		allowed[clean] = true
		dirSet[filepath.Dir(clean)] = true
	}
	if len(dirSet) == 0 {
		return map[string]transcriptBodyMatch{}, nil
	}
	dirs := make([]string, 0, len(dirSet))
	for dir := range dirSet {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	args := append([]string{
		"--files-with-matches", "--null", "--fixed-strings", "--ignore-case",
		"--no-messages", "--glob", "*.md", "--max-filesize", "8M", "--",
		strings.TrimSpace(query),
	}, dirs...)
	out, err := matchingTranscriptFiles(ctx, args)
	if err != nil {
		return nil, err
	}
	matches := make(map[string]transcriptBodyMatch)
	for _, rawPath := range bytes.Split(out, []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		path := filepath.Clean(string(rawPath))
		if !allowed[path] {
			continue
		}
		match, err := visibleTranscriptMatches(ctx, path, query)
		if err != nil {
			return nil, err
		}
		if match.count > 0 {
			matches[path] = match
		}
	}
	return matches, nil
}

const (
	transcriptSearchMaxPathOutput = 4 * 1024 * 1024
	// Files are already capped at 8 MiB. Reading one line from one file at a
	// time preserves complete literal matching without rg's JSON amplification.
	transcriptSearchMaxLine = 8 * 1024 * 1024
)

var errTranscriptSearchOutputTooLarge = errors.New("transcript search output too large")

var transcriptRoleHeading = regexp.MustCompile(`^## (User|Agent|Agent's Thoughts) \([^)]*\)\r?$`)

func matchingTranscriptFiles(ctx context.Context, args []string) ([]byte, error) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := commandContext(childCtx, "rg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, transcriptSearchMaxPathOutput+1))
	if readErr != nil {
		cancel()
		_ = cmd.Wait()
		return nil, readErr
	}
	if len(out) > transcriptSearchMaxPathOutput {
		cancel()
		_ = cmd.Wait()
		return nil, errTranscriptSearchOutputTooLarge
	}
	waitErr := cmd.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, waitErr
		}
	}
	return out, nil
}

func visibleTranscriptMatches(ctx context.Context, path, query string) (transcriptBodyMatch, error) {
	file, err := os.Open(path)
	if err != nil {
		return transcriptBodyMatch{}, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	needle := strings.ToLower(strings.TrimSpace(query))
	visible := false
	lineNumber := 0
	match := transcriptBodyMatch{}
	for {
		line, tooLong, err := readBoundedTranscriptLine(reader, transcriptSearchMaxLine)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return transcriptBodyMatch{}, err
		}
		lineNumber++
		if err := ctx.Err(); err != nil {
			return transcriptBodyMatch{}, err
		}
		if tooLong {
			continue
		}
		text := string(line)
		if heading := transcriptRoleHeading.FindStringSubmatch(text); heading != nil {
			visible = heading[1] != "Agent's Thoughts"
			continue
		}
		if !visible {
			continue
		}
		count := strings.Count(strings.ToLower(text), needle)
		if count == 0 {
			continue
		}
		if match.snippet == "" {
			match.snippet = compactTranscriptSnippet(text, query)
			match.line = lineNumber
		}
		match.count += count
	}
	return match, nil
}

func readBoundedTranscriptLine(reader *bufio.Reader, maxBytes int) ([]byte, bool, error) {
	var line []byte
	tooLong := false
	for {
		fragment, more, err := reader.ReadLine()
		if err != nil {
			return nil, tooLong, err
		}
		if !tooLong {
			if len(line)+len(fragment) > maxBytes {
				line = nil
				tooLong = true
			} else {
				line = append(line, fragment...)
			}
		}
		if !more {
			return line, tooLong, nil
		}
	}
}

func compactTranscriptSnippet(s, query string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	const limit = 240
	if len(runes) <= limit {
		return s
	}
	lower := strings.ToLower(s)
	matchByte := strings.Index(lower, strings.ToLower(strings.TrimSpace(query)))
	matchRune := 0
	if matchByte >= 0 {
		matchRune = len([]rune(lower[:matchByte]))
	}
	queryRunes := len([]rune(strings.TrimSpace(query)))
	start := matchRune - (limit-queryRunes)/2
	if start < 0 {
		start = 0
	}
	end := start + limit
	if end > len(runes) {
		end = len(runes)
		start = end - limit
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(runes) {
		suffix = "…"
	}
	return prefix + strings.TrimSpace(string(runes[start:end])) + suffix
}

func allowedTranscriptPath(path, home string) bool {
	clean := filepath.Clean(path)
	sep := string(filepath.Separator)
	return filepath.IsAbs(clean) &&
		strings.HasPrefix(clean, filepath.Clean(home)+sep) &&
		strings.Contains(clean, sep+".agent-shell"+sep+"transcripts"+sep) &&
		strings.HasSuffix(clean, ".md")
}

func eligibleTranscriptRecords(transcripts []transcriptInfo) ([]transcriptInfo, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	eligible := make([]transcriptInfo, 0, len(transcripts))
	for _, transcript := range transcripts {
		clean := filepath.Clean(transcript.File)
		if !allowedTranscriptPath(clean, home) && !allowedTranscriptPath(clean, realHome) {
			continue
		}
		realPath, err := filepath.EvalSymlinks(clean)
		if err != nil || !allowedTranscriptPath(realPath, realHome) {
			continue
		}
		info, err := os.Stat(realPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 8*1024*1024 {
			continue
		}
		transcript.File = realPath
		eligible = append(eligible, transcript)
	}
	return eligible, nil
}

type transcriptIndexLoader func(ctx context.Context, limit int) ([]transcriptInfo, error)

func newTranscriptSearchHandler(load transcriptIndexLoader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var req struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		query := strings.TrimSpace(req.Query)
		queryLen := len([]rune(query))
		if queryLen < 2 || queryLen > 200 || strings.IndexFunc(query, unicode.IsControl) >= 0 {
			http.Error(w, "query must be between 2 and 200 printable characters", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		transcripts, err := load(ctx, 0)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				http.Error(w, "search timed out", http.StatusGatewayTimeout)
				return
			}
			http.Error(w, "transcript index unavailable", http.StatusBadGateway)
			return
		}
		transcripts = mergeTranscriptLabels(transcripts, loadLabels())
		results, truncated, err := searchTranscriptRecords(ctx, query, transcripts)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				http.Error(w, "search timed out", http.StatusGatewayTimeout)
				return
			}
			log.Printf("transcript search: %v", err)
			http.Error(w, "search failed", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"query":     query,
			"results":   results,
			"truncated": truncated,
		})
	})
}

// unquoteElispBase64 strips the quotes from a prin1-printed elisp string
// and base64-decodes the body.  The elisp side ASCII-armors its JSON
// because emacsclient octal-escapes non-ASCII in printed strings.
func unquoteElispBase64(s string) (string, error) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("not an elisp string: %.40s", s)
	}
	data, err := base64.StdEncoding.DecodeString(s[1 : len(s)-1])
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func decodeTranscriptIndexOutputContext(ctx context.Context, out []byte) ([]transcriptInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	jsonString, err := unquoteElispBase64(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var transcripts []transcriptInfo
	if err := json.Unmarshal([]byte(jsonString), &transcripts); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return transcripts, nil
}

func decodeTranscriptIndexOutput(out []byte) ([]transcriptInfo, error) {
	return decodeTranscriptIndexOutputContext(context.Background(), out)
}

func loadTranscriptIndex(ctx context.Context, limit int) ([]transcriptInfo, error) {
	expr := fmt.Sprintf("(syzygy-recall-transcripts-json %d)", limit)
	out, err := evalEmacsContext(ctx, "emacsclient", "--eval", expr)
	if err != nil {
		return nil, fmt.Errorf("emacsclient: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return decodeTranscriptIndexOutputContext(ctx, out)
}

func handleTranscriptSearch(w http.ResponseWriter, r *http.Request) {
	newTranscriptSearchHandler(loadTranscriptIndex).ServeHTTP(w, r)
}

func newTranscriptsHandler(load transcriptIndexLoader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		transcripts, err := load(r.Context(), 100)
		if err != nil {
			log.Printf("transcripts: %v", err)
			http.Error(w, "history unavailable", http.StatusBadGateway)
			return
		}
		transcripts = mergeTranscriptLabels(transcripts, loadLabels())
		transcripts, err = eligibleTranscriptRecords(transcripts)
		if err != nil {
			log.Printf("transcripts: %v", err)
			http.Error(w, "history unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(transcripts)
	})
}

func handleTranscripts(w http.ResponseWriter, r *http.Request) {
	newTranscriptsHandler(loadTranscriptIndex).ServeHTTP(w, r)
}

func newTranscriptHandler(load transcriptIndexLoader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		transcripts, err := load(ctx, 0)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				http.Error(w, "transcript lookup timed out", http.StatusGatewayTimeout)
				return
			}
			http.Error(w, "transcript index unavailable", http.StatusBadGateway)
			return
		}
		indexed, err := eligibleTranscriptRecords(transcripts)
		if err != nil {
			http.Error(w, "transcript index unavailable", http.StatusBadGateway)
			return
		}
		requested, err := eligibleTranscriptRecords([]transcriptInfo{{File: r.URL.Query().Get("file")}})
		if err != nil || len(requested) != 1 {
			http.Error(w, "path not allowed", http.StatusForbidden)
			return
		}
		allowed := false
		for _, transcript := range indexed {
			if transcript.File == requested[0].File {
				allowed = true
				break
			}
		}
		if !allowed {
			http.Error(w, "path not allowed", http.StatusForbidden)
			return
		}
		data, err := os.ReadFile(requested[0].File)
		if err != nil {
			http.Error(w, "transcript unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"content": string(data)})
	})
}

func handleTranscript(w http.ResponseWriter, r *http.Request) {
	newTranscriptHandler(loadTranscriptIndex).ServeHTTP(w, r)
}

// handleLabel sets/clears a convo label via the Emacs daemon — same
// bridge as handleKill.  Emacs owns the label hash and mirrors it to
// labels.json, so rig and phone always agree.
func handleLabel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		BufferName string `json:"bufferName"`
		Label      string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.BufferName == "" || !validBufferName.MatchString(req.BufferName) {
		http.Error(w, "invalid buffer name", http.StatusBadRequest)
		return
	}
	if len(req.Label) > 80 {
		http.Error(w, "label too long", http.StatusBadRequest)
		return
	}

	esc := func(v string) string {
		v = strings.ReplaceAll(v, `\`, `\\`)
		return strings.ReplaceAll(v, `"`, `\"`)
	}
	expr := fmt.Sprintf(`(mr-x/agent-label-set "%s" "%s")`, esc(req.BufferName), esc(req.Label))

	out, err := evalEmacs("emacsclient", "--eval", expr)
	if err != nil {
		log.Printf("label: %v: %s", err, out)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": strings.TrimSpace(string(out))})
		return
	}
	if strings.TrimSpace(string(out)) == "nil" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "no such buffer"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// evalEmacs runs an emacsclient-backed command, retrying when the eval
// dies with "*ERROR*: Quit": a desk-side C-g/ESC aborts whatever elisp
// is mid-flight — server evals from this bridge included — and a beat
// later the daemon is fine again.
func evalEmacs(name string, args ...string) ([]byte, error) {
	return evalEmacsContext(context.Background(), name, args...)
}

func evalEmacsContext(ctx context.Context, name string, args ...string) ([]byte, error) {
	var out []byte
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		out, err = commandContext(ctx, name, args...).CombinedOutput()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}
		if err == nil || !bytes.Contains(out, []byte("*ERROR*: Quit")) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return out, err
}

func handleKill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		BufferName string `json:"bufferName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.BufferName == "" {
		http.Error(w, "bufferName is required", http.StatusBadRequest)
		return
	}

	// Validate buffer name: only allow characters that appear in agent-shell names
	// (e.g. "Claude Code Agent @ myproject<2>")
	if !validBufferName.MatchString(req.BufferName) {
		http.Error(w, "invalid buffer name", http.StatusBadRequest)
		return
	}

	// Escape for elisp string
	escaped := strings.ReplaceAll(req.BufferName, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	expr := fmt.Sprintf(`(meta-agent-shell-close-session "%s")`, escaped)

	out, err := evalEmacs("emacsclient", "--eval", expr)
	if err != nil {
		log.Printf("kill: %v: %s", err, out)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out)))})
		return
	}

	result := strings.TrimSpace(string(out))
	if result == "nil" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "session not found or already closed"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func probeSocket(sockPath string, pid int) sessionInfo {
	info := sessionInfo{Pid: pid}

	info.Cwd = processCwd(pid)
	if info.Cwd != "" {
		info.Project = filepath.Base(info.Cwd)
	}

	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		return info
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(500 * time.Millisecond))

	// Read raw bytes instead of scanning lines — much faster for large replays.
	// We only need the first ~2 response messages (initialize + session/new).
	buf := make([]byte, 64*1024)
	var data []byte
	var totalRead int
	gotSessionID := false
	gotTitle := false

	for {
		n, err := conn.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
			// Parse complete lines we have so far
			for {
				nl := bytes.IndexByte(data, '\n')
				if nl < 0 {
					break
				}
				line := data[:nl]
				data = data[nl+1:]

				if len(line) == 0 {
					continue
				}

				var msg struct {
					Result json.RawMessage `json:"result"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if json.Unmarshal(line, &msg) != nil {
					continue
				}

				// Check for acp-multiplex/meta notification
				if msg.Method == "acp-multiplex/meta" && msg.Params != nil {
					var meta struct {
						Name string `json:"name"`
					}
					if json.Unmarshal(msg.Params, &meta) == nil && meta.Name != "" {
						info.BufferName = meta.Name
					}
					continue
				}

				if msg.Method == "session/update" && msg.Params != nil {
					var upd struct {
						SessionID string `json:"sessionId"`
						Update    struct {
							SessionUpdate string `json:"sessionUpdate"`
							Content       struct {
								Text string `json:"text"`
							} `json:"content"`
						} `json:"update"`
					}
					if json.Unmarshal(msg.Params, &upd) == nil {
						// Resumed convos (session/load, session/resume) never
						// replay a session/new response — acp-multiplex only
						// caches initialize + session/new — so the id only
						// rides on notifications.  Without it, labels.json and
						// status.json (both keyed by session id) never match.
						if !gotSessionID && upd.SessionID != "" {
							info.SessionID = upd.SessionID
							gotSessionID = true
						}
						// First user message in the replay becomes the card
						// headline (replay coalesces chunks, so one
						// notification ≈ one full prompt).
						if info.Preview == "" &&
							upd.Update.SessionUpdate == "user_message_chunk" &&
							upd.Update.Content.Text != "" {
							preview := strings.Join(strings.Fields(upd.Update.Content.Text), " ")
							if len(preview) > 120 {
								preview = preview[:120]
							}
							info.Preview = preview
						}
					}
					continue
				}

				if msg.Result == nil {
					continue
				}

				var res struct {
					AgentInfo *struct {
						Title string `json:"title"`
						Name  string `json:"name"`
					} `json:"agentInfo"`
					SessionID string `json:"sessionId"`
					Cwd       string `json:"cwd"`
				}
				if json.Unmarshal(msg.Result, &res) != nil {
					continue
				}

				if res.AgentInfo != nil {
					info.Title = res.AgentInfo.Title
					if info.Title == "" {
						info.Title = res.AgentInfo.Name
					}
					gotTitle = true
				}
				if res.SessionID != "" {
					info.SessionID = res.SessionID
					gotSessionID = true
				}
				if res.Cwd != "" {
					info.Cwd = res.Cwd
					info.Project = filepath.Base(info.Cwd)
				}

				if gotSessionID && gotTitle && info.BufferName != "" && info.Preview != "" {
					return info
				}
			}
		}
		totalRead += n
		// The handshake and first prompt live at the top of the replay;
		// don't chew through megabytes of history for a card headline.
		if totalRead > 512*1024 {
			break
		}
		if err != nil {
			break
		}
	}

	return info
}

func processCwd(pid int) string {
	// Try /proc/<pid>/cwd first (Linux)
	if target, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		return target
	}
	// Fall back to lsof (macOS)
	out, err := command("lsof", "-a", "-d", "cwd", "-p", strconv.Itoa(pid), "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			return line[1:]
		}
	}
	return ""
}

// --- Reverse call handlers ---

// handleReverseCall checks if a JSON line from the socket is a reverse call
// (fs/read_text_file, fs/write_text_file) that we can handle locally.
// Returns the JSON-RPC response to send back, or nil if not a handled method.
func handleReverseCall(line []byte) []byte {
	var env struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &env) != nil || env.Method == "" || env.ID == nil {
		return nil
	}

	switch env.Method {
	case "fs/read_text_file":
		return handleFsRead(env.ID, env.Params)
	case "fs/write_text_file":
		return handleFsWrite(env.ID, env.Params)
	default:
		return nil
	}
}

func handleFsRead(id json.RawMessage, params json.RawMessage) []byte {
	var p struct {
		Path  string `json:"path"`
		Line  *int   `json:"line"`
		Limit *int   `json:"limit"`
	}
	if json.Unmarshal(params, &p) != nil || p.Path == "" {
		return jsonRPCError(id, -32602, "invalid params")
	}

	data, err := os.ReadFile(p.Path)
	if err != nil {
		return jsonRPCError(id, -32000, err.Error())
	}

	content := string(data)

	// Handle line/limit offset (1-based line numbers)
	if p.Line != nil || p.Limit != nil {
		lines := strings.Split(content, "\n")
		start := 0
		if p.Line != nil && *p.Line > 1 {
			start = *p.Line - 1
		}
		if start > len(lines) {
			start = len(lines)
		}
		lines = lines[start:]
		if p.Limit != nil && *p.Limit < len(lines) {
			lines = lines[:*p.Limit]
		}
		content = strings.Join(lines, "\n")
	}

	resp, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  map[string]string{"content": content},
	})
	return resp
}

func handleFsWrite(id json.RawMessage, params json.RawMessage) []byte {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if json.Unmarshal(params, &p) != nil || p.Path == "" {
		return jsonRPCError(id, -32602, "invalid params")
	}

	if err := os.WriteFile(p.Path, []byte(p.Content), 0644); err != nil {
		return jsonRPCError(id, -32000, err.Error())
	}

	resp, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  map[string]interface{}{},
	})
	return resp
}

func jsonRPCError(id json.RawMessage, code int, message string) []byte {
	resp, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]interface{}{"code": code, "message": message},
	})
	return resp
}

// --- WebSocket bridge ---

// bridgeWebSocket connects to the proxy socket and bridges to the browser.
// Reads the replay, keeping only responses and the last N notifications,
// then forwards live traffic.
// Phone-driven turns: the Emacs status sidecar can't see turns the
// phone starts (Emacs isn't the driver), but every one of them flows
// through this bridge — track session/prompt requests and their
// responses so /api/sessions can report "busy" for them too.
var phoneTurns = struct {
	mu sync.Mutex
	m  map[string]int
}{m: map[string]int{}}

func phoneTurnStart(sid string) {
	phoneTurns.mu.Lock()
	defer phoneTurns.mu.Unlock()
	phoneTurns.m[sid]++
}

func phoneTurnEnd(sid string) {
	phoneTurns.mu.Lock()
	defer phoneTurns.mu.Unlock()
	if phoneTurns.m[sid] <= 1 {
		delete(phoneTurns.m, sid)
	} else {
		phoneTurns.m[sid]--
	}
}

func bridgeWebSocket(ws *websocket.Conn, sockPath string) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Printf("ws: connect to %s: %v", sockPath, err)
		errMsg := fmt.Sprintf(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Connection failed","data":{"details":"%s"}}}`, strings.ReplaceAll(err.Error(), `"`, `\"`))
		websocket.Message.Send(ws, errMsg)
		ws.Close()
		return
	}
	defer conn.Close()

	// Read the replay into memory using a short idle timeout to detect
	// when the replay burst is done (no explicit end marker from the proxy).
	var responses [][]byte
	var notifications [][]byte

	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 256*1024)

	for {
		// Short deadline: if no data arrives within 150ms, replay is done.
		conn.SetDeadline(time.Now().Add(150 * time.Millisecond))
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				nl := bytes.IndexByte(buf, '\n')
				if nl < 0 {
					break
				}
				line := make([]byte, nl)
				copy(line, buf[:nl])
				buf = buf[nl+1:]
				if len(line) == 0 {
					continue
				}
				if bytes.Contains(line, []byte(`"result"`)) || bytes.Contains(line, []byte(`"error"`)) {
					responses = append(responses, line)
				} else {
					notifications = append(notifications, line)
				}
			}
		}
		if err != nil {
			break // timeout or EOF — replay is done
		}
	}

	conn.SetDeadline(time.Time{})

	// Send full replay: all responses + all notifications
	for _, line := range responses {
		websocket.Message.Send(ws, string(line))
	}
	for _, line := range notifications {
		websocket.Message.Send(ws, string(line))
	}

	log.Printf("ws: replay %d responses + %d notifications",
		len(responses), len(notifications))

	// Bridge live traffic
	var brMu sync.Mutex
	inflight := map[string]string{} // request id -> sessionId (this bridge's prompts)
	var once sync.Once
	closeAll := func() {
		ws.Close()
		conn.Close()
		// If the phone vanishes mid-turn its response never routes back;
		// don't leave the session stuck "busy".
		brMu.Lock()
		for _, sid := range inflight {
			phoneTurnEnd(sid)
		}
		inflight = map[string]string{}
		brMu.Unlock()
	}

	go func() {
		defer once.Do(closeAll)
		for {
			var msg string
			if err := websocket.Message.Receive(ws, &msg); err != nil {
				if err != io.EOF {
					log.Printf("ws recv: %v", err)
				}
				return
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					SessionID string `json:"sessionId"`
				} `json:"params"`
			}
			if json.Unmarshal([]byte(msg), &req) == nil &&
				req.Method == "session/prompt" &&
				req.Params.SessionID != "" && len(req.ID) > 0 {
				brMu.Lock()
				inflight[string(req.ID)] = req.Params.SessionID
				brMu.Unlock()
				phoneTurnStart(req.Params.SessionID)
			}
			conn.Write([]byte(msg))
			conn.Write([]byte("\n"))
		}
	}()

	func() {
		defer once.Do(closeAll)
		// forwardOrHandle checks if a line is a reverse call we handle locally.
		// If so, sends the response back to the socket. Otherwise forwards to browser.
		forwardOrHandle := func(line []byte) {
			if resp := handleReverseCall(line); resp != nil {
				conn.Write(resp)
				conn.Write([]byte("\n"))
				return
			}
			var rsp struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(line, &rsp) == nil &&
				rsp.Method == "" && len(rsp.ID) > 0 {
				brMu.Lock()
				if sid, ok := inflight[string(rsp.ID)]; ok {
					delete(inflight, string(rsp.ID))
					phoneTurnEnd(sid)
				}
				brMu.Unlock()
			}
			websocket.Message.Send(ws, string(line))
		}
		// Flush leftover bytes from replay read
		for {
			nl := bytes.IndexByte(buf, '\n')
			if nl < 0 {
				break
			}
			if nl > 0 {
				forwardOrHandle(buf[:nl])
			}
			buf = buf[nl+1:]
		}
		for {
			n, err := conn.Read(tmp)
			if err != nil {
				if err != io.EOF {
					log.Printf("ws sock read: %v", err)
				}
				return
			}
			buf = append(buf, tmp[:n]...)
			for {
				nl := bytes.IndexByte(buf, '\n')
				if nl < 0 {
					break
				}
				if nl > 0 {
					forwardOrHandle(buf[:nl])
				}
				buf = buf[nl+1:]
			}
		}
	}()
}

// --- File browser ---

// allowedRoots returns the cwd of each active session.
func allowedRoots() []string {
	socks := discoverSockets()
	seen := map[string]bool{}
	var roots []string
	for _, s := range socks {
		cwd := processCwd(s.pid)
		if cwd != "" && !seen[cwd] {
			seen[cwd] = true
			roots = append(roots, cwd)
		}
	}
	return roots
}

// isUnderRoots checks if absPath is under one of the allowed roots.
// Resolves symlinks to prevent traversal via symlinked directories.
func isUnderRoots(absPath string, roots []string) bool {
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return false
	}
	for _, root := range roots {
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rootResolved = filepath.Clean(rootResolved) + string(filepath.Separator)
		if strings.HasPrefix(resolved+string(filepath.Separator), rootResolved) {
			return true
		}
	}
	return false
}

type fileEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size,omitempty"`
}

func handleFileList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path       string `json:"path"`
		ShowHidden bool   `json:"showHidden"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	dirPath := req.Path
	if dirPath == "" {
		dirPath = "."
	}

	absPath, err := filepath.Abs(dirPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if !isUnderRoots(absPath, allowedRoots()) {
		http.Error(w, "path not allowed", http.StatusForbidden)
		return
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	showHidden := req.ShowHidden
	var files []fileEntry
	for _, e := range entries {
		name := e.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fe := fileEntry{Name: name, IsDir: e.IsDir()}
		if !e.IsDir() {
			fe.Size = info.Size()
		}
		files = append(files, fe)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"path":  absPath,
		"files": files,
	})
}

func handleFileRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	filePath := req.Path
	if filePath == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if !isUnderRoots(absPath, allowedRoots()) {
		http.Error(w, "path not allowed", http.StatusForbidden)
		return
	}

	info, err := os.Stat(absPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}
	if info.Size() > 1024*1024 {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"path":    absPath,
		"content": string(data),
	})
}
