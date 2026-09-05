package main

// Web Push: acp-mobile sends its own iOS notifications so a tap opens the
// home-screen web app instead of a browser.  Emacs (agent-shell-push.el)
// calls POST /api/notify on localhost; the phone subscribes through the
// bell in index.html via /api/push-key + /api/push-subscribe; sw.js shows
// the notification and routes the tap back into the running page.

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// vapidSubject is the VAPID `sub` claim push services may use to contact
// the sender.  Apple requires a mailto: or https: URL.
const vapidSubject = "https://github.com/mrx-xo/acp-mobile"

// pushTTL is how long a push service holds an undelivered notification.
const pushTTL = 60 * 60

type vapidKeys struct {
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"`
}

var vapidState struct {
	mu   sync.Mutex
	keys *vapidKeys
}

// subsMu serializes reads and writes of push-subscriptions.json.
var subsMu sync.Mutex

// webpushSend is the network seam; tests replace it so no push service is
// ever contacted.
var webpushSend = webpush.SendNotificationWithContext

func resetWebPushState() {
	vapidState.mu.Lock()
	vapidState.keys = nil
	vapidState.mu.Unlock()
}

func acpMobileDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".acp-mobile")
}

func vapidPath() string {
	return filepath.Join(acpMobileDir(), "vapid.json")
}

func subscriptionsPath() string {
	return filepath.Join(acpMobileDir(), "push-subscriptions.json")
}

// loadOrCreateVAPID returns the VAPID keypair, generating and persisting
// one (0600) on first use.  Rotating the keypair invalidates every phone
// subscription, so it is only ever created, never regenerated.
func loadOrCreateVAPID() (vapidKeys, error) {
	vapidState.mu.Lock()
	defer vapidState.mu.Unlock()
	if vapidState.keys != nil {
		return *vapidState.keys, nil
	}
	p := vapidPath()
	var keys vapidKeys
	if data, err := os.ReadFile(p); err == nil {
		if json.Unmarshal(data, &keys) == nil && keys.PublicKey != "" && keys.PrivateKey != "" {
			vapidState.keys = &keys
			return keys, nil
		}
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return vapidKeys{}, err
	}
	keys = vapidKeys{PublicKey: pub, PrivateKey: priv}
	data, err := json.Marshal(keys)
	if err != nil {
		return vapidKeys{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return vapidKeys{}, err
	}
	if err := os.WriteFile(p, append(data, '\n'), 0600); err != nil {
		return vapidKeys{}, err
	}
	log.Printf("webpush: generated VAPID keypair at %s", p)
	vapidState.keys = &keys
	return keys, nil
}

// loadSubscriptions returns stored PushSubscriptions keyed by endpoint.
// Missing or corrupt file means nobody is subscribed.
func loadSubscriptions() map[string]webpush.Subscription {
	subsMu.Lock()
	defer subsMu.Unlock()
	return readSubscriptions()
}

func readSubscriptions() map[string]webpush.Subscription {
	subs := map[string]webpush.Subscription{}
	data, err := os.ReadFile(subscriptionsPath())
	if err != nil {
		return subs
	}
	if json.Unmarshal(data, &subs) != nil {
		return map[string]webpush.Subscription{}
	}
	return subs
}

func writeSubscriptions(subs map[string]webpush.Subscription) error {
	p := subscriptionsPath()
	data, err := json.MarshalIndent(subs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0600)
}

func addSubscription(sub webpush.Subscription) error {
	subsMu.Lock()
	defer subsMu.Unlock()
	subs := readSubscriptions()
	subs[sub.Endpoint] = sub
	return writeSubscriptions(subs)
}

func removeSubscription(endpoint string) error {
	subsMu.Lock()
	defer subsMu.Unlock()
	subs := readSubscriptions()
	if _, ok := subs[endpoint]; !ok {
		return nil
	}
	delete(subs, endpoint)
	return writeSubscriptions(subs)
}

func removeSubscriptions(endpoints []string) error {
	if len(endpoints) == 0 {
		return nil
	}
	subsMu.Lock()
	defer subsMu.Unlock()
	subs := readSubscriptions()
	for _, ep := range endpoints {
		delete(subs, ep)
	}
	return writeSubscriptions(subs)
}

func validSubscription(sub webpush.Subscription) error {
	if !strings.HasPrefix(sub.Endpoint, "https://") {
		return errors.New("endpoint must be an https URL")
	}
	if sub.Keys.Auth == "" || sub.Keys.P256dh == "" {
		return errors.New("subscription keys missing")
	}
	return nil
}

// GET /api/push-key -> {"publicKey": <base64url VAPID public key>}
func handlePushKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	keys, err := loadOrCreateVAPID()
	if err != nil {
		log.Printf("webpush: vapid: %v", err)
		http.Error(w, "vapid keys unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"publicKey": keys.PublicKey})
}

// POST /api/push-subscribe {subscription} stores a PushSubscription.
// POST {unsubscribe:true, subscription:{endpoint}} or DELETE removes it.
func handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Subscription webpush.Subscription `json:"subscription"`
		Unsubscribe  bool                 `json:"unsubscribe"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodDelete || req.Unsubscribe {
		if req.Subscription.Endpoint == "" {
			http.Error(w, "endpoint required", http.StatusBadRequest)
			return
		}
		if err := removeSubscription(req.Subscription.Endpoint); err != nil {
			log.Printf("webpush: unsubscribe: %v", err)
			http.Error(w, "could not save subscriptions", http.StatusInternalServerError)
			return
		}
		log.Printf("webpush: unsubscribed %s", shortEndpoint(req.Subscription.Endpoint))
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "subscribed": false})
		return
	}
	if err := validSubscription(req.Subscription); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := addSubscription(req.Subscription); err != nil {
		log.Printf("webpush: subscribe: %v", err)
		http.Error(w, "could not save subscription", http.StatusInternalServerError)
		return
	}
	log.Printf("webpush: subscribed %s", shortEndpoint(req.Subscription.Endpoint))
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "subscribed": true})
}

// shortEndpoint trims a push endpoint for logs; the full URL is a
// capability and does not belong in log files.
func shortEndpoint(ep string) string {
	if i := strings.Index(ep, "/"); i >= 0 {
		if j := strings.Index(ep[i+2:], "/"); j >= 0 {
			return ep[:i+2+j] + "/..." + ep[max(len(ep)-8, i+2+j):]
		}
	}
	return ep
}

// POST /api/notify {bufferName, title, message}: fan out one Web Push per
// stored subscription.  Called by Emacs on localhost; the authkey check in
// the outer handler already gates it.
func handleNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		BufferName string `json:"bufferName"`
		Title      string `json:"title"`
		Message    string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.BufferName == "" || !validBufferName.MatchString(req.BufferName) {
		http.Error(w, "invalid buffer name", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		req.Title = req.BufferName
	}
	sent, dropped, err := notifyAll(r.Context(), req.Title, req.Message, req.BufferName)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		log.Printf("webpush: notify: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	log.Printf("webpush: %q -> %d sent, %d dropped (%s)", req.Message, sent, dropped, req.BufferName)
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "sent": sent, "dropped": dropped})
}

// notifyAll sends {title, body, bufferName, tag} to every subscription and
// forgets the ones the push service reports gone (404/410).
func notifyAll(ctx context.Context, title, body, bufferName string) (sent, dropped int, err error) {
	keys, err := loadOrCreateVAPID()
	if err != nil {
		return 0, 0, err
	}
	subs := loadSubscriptions()
	if len(subs) == 0 {
		return 0, 0, nil
	}
	payload, err := json.Marshal(map[string]string{
		"title":      title,
		"body":       body,
		"bufferName": bufferName,
		"tag":        bufferName,
	})
	if err != nil {
		return 0, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var gone []string
	for endpoint, sub := range subs {
		sub := sub
		resp, sendErr := webpushSend(ctx, payload, &sub, &webpush.Options{
			Subscriber:      vapidSubject,
			VAPIDPublicKey:  keys.PublicKey,
			VAPIDPrivateKey: keys.PrivateKey,
			TTL:             pushTTL,
			Urgency:         webpush.UrgencyHigh,
		})
		if sendErr != nil {
			log.Printf("webpush: send %s: %v", shortEndpoint(endpoint), sendErr)
			continue
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			gone = append(gone, endpoint)
			dropped++
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			sent++
		default:
			log.Printf("webpush: send %s: HTTP %d", shortEndpoint(endpoint), resp.StatusCode)
		}
	}
	if err := removeSubscriptions(gone); err != nil {
		log.Printf("webpush: prune: %v", err)
	}
	return sent, dropped, nil
}

//go:embed sw.js
var serviceWorkerJS []byte

//go:embed manifest.webmanifest
var manifestJSON []byte

// GET /sw.js: root scope so the worker controls "/", never cached so a
// rebuilt binary ships a fresh worker on the next update check.
func handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(serviceWorkerJS)
}

// GET /manifest.webmanifest: iOS needs a manifest (display: standalone)
// before it grants Web Push to a home-screen web app.
func handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(manifestJSON)
}

// serveURLFromStatus returns the https origin that `tailscale serve`
// proxies to 127.0.0.1:PORT, or "" when there is none.  STATUS is the
// output of `tailscale serve status --json`.
func serveURLFromStatus(status []byte, port string) string {
	var st struct {
		Web map[string]struct {
			Handlers map[string]struct {
				Proxy string `json:"Proxy"`
			} `json:"Handlers"`
		} `json:"Web"`
	}
	if json.Unmarshal(status, &st) != nil {
		return ""
	}
	want := "127.0.0.1:" + port
	for hostPort, site := range st.Web {
		h, ok := site.Handlers["/"]
		if !ok {
			continue
		}
		u, err := url.Parse(h.Proxy)
		if err != nil || u.Host != want {
			continue
		}
		origin := "https://" + hostPort
		return strings.TrimSuffix(origin, ":443")
	}
	return ""
}

// tailscaleServeURL asks the tailscale CLI whether an https proxy to this
// port exists.  Web Push needs a secure origin, and the home-screen app
// must be installed from it, so the link file hands out that URL.
func tailscaleServeURL(port string) string {
	out, err := command("tailscale", "serve", "status", "--json").Output()
	if err != nil {
		return ""
	}
	return serveURLFromStatus(out, port)
}

// linkURL is what ~/.acp-mobile/link holds: the https serve origin when
// one exists, else the plain tailnet (or loopback) http URL.
func linkURL(serveOrigin, tailnetHost, port, authKey string) string {
	switch {
	case serveOrigin != "":
		return fmt.Sprintf("%s?authkey=%s", serveOrigin, authKey)
	case tailnetHost != "":
		return fmt.Sprintf("http://%s:%s?authkey=%s", tailnetHost, port, authKey)
	default:
		return fmt.Sprintf("http://127.0.0.1:%s?authkey=%s", port, authKey)
	}
}
