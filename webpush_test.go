package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetWebPushState()
	return home
}

func TestLoadOrCreateVAPIDPersistsPrivateKeypair(t *testing.T) {
	home := isolatedHome(t)
	first, err := loadOrCreateVAPID()
	if err != nil {
		t.Fatal(err)
	}
	if first.PublicKey == "" || first.PrivateKey == "" {
		t.Fatalf("empty keypair: %+v", first)
	}
	info, err := os.Stat(filepath.Join(home, ".acp-mobile", "vapid.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("vapid.json mode = %o, want 0600", info.Mode().Perm())
	}
	second, err := loadOrCreateVAPID()
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second load regenerated keys: %+v vs %+v", second, first)
	}
}

func TestSubscriptionStoreKeyedByEndpoint(t *testing.T) {
	home := isolatedHome(t)
	a := webpush.Subscription{Endpoint: "https://push.example/a", Keys: webpush.Keys{Auth: "a1", P256dh: "p1"}}
	b := webpush.Subscription{Endpoint: "https://push.example/b", Keys: webpush.Keys{Auth: "b1", P256dh: "p2"}}
	if err := addSubscription(a); err != nil {
		t.Fatal(err)
	}
	if err := addSubscription(b); err != nil {
		t.Fatal(err)
	}
	// Re-adding the same endpoint replaces, never duplicates.
	a.Keys.Auth = "a2"
	if err := addSubscription(a); err != nil {
		t.Fatal(err)
	}
	subs := loadSubscriptions()
	if len(subs) != 2 {
		t.Fatalf("got %d subscriptions, want 2: %+v", len(subs), subs)
	}
	if subs[a.Endpoint].Keys.Auth != "a2" {
		t.Fatalf("re-add did not replace keys: %+v", subs[a.Endpoint])
	}
	info, err := os.Stat(filepath.Join(home, ".acp-mobile", "push-subscriptions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("subscriptions mode = %o, want 0600", info.Mode().Perm())
	}
	if err := removeSubscription(a.Endpoint); err != nil {
		t.Fatal(err)
	}
	subs = loadSubscriptions()
	if _, ok := subs[a.Endpoint]; ok || len(subs) != 1 {
		t.Fatalf("remove failed: %+v", subs)
	}
}

func TestLoadSubscriptionsMissingFileIsEmpty(t *testing.T) {
	isolatedHome(t)
	if subs := loadSubscriptions(); len(subs) != 0 {
		t.Fatalf("missing file should yield no subscriptions, got %+v", subs)
	}
}

func TestHandlePushKeyReturnsVAPIDPublicKey(t *testing.T) {
	isolatedHome(t)
	keys, err := loadOrCreateVAPID()
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handlePushKey(w, httptest.NewRequest(http.MethodGet, "/api/push-key", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	var resp struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.PublicKey != keys.PublicKey {
		t.Fatalf("publicKey = %q, want %q", resp.PublicKey, keys.PublicKey)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestHandlePushSubscribeStoresAndUnsubscribes(t *testing.T) {
	isolatedHome(t)
	body := `{"subscription":{"endpoint":"https://push.example/x","keys":{"auth":"a","p256dh":"p"}}}`
	w := httptest.NewRecorder()
	handlePushSubscribe(w, httptest.NewRequest(http.MethodPost, "/api/push-subscribe", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("subscribe status = %d: %s", w.Code, w.Body)
	}
	if subs := loadSubscriptions(); subs["https://push.example/x"].Keys.P256dh != "p" {
		t.Fatalf("subscription not stored: %+v", subs)
	}

	w = httptest.NewRecorder()
	handlePushSubscribe(w, httptest.NewRequest(http.MethodPost, "/api/push-subscribe",
		strings.NewReader(`{"unsubscribe":true,"subscription":{"endpoint":"https://push.example/x"}}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("unsubscribe status = %d: %s", w.Code, w.Body)
	}
	if subs := loadSubscriptions(); len(subs) != 0 {
		t.Fatalf("unsubscribe left %+v", subs)
	}
}

func TestHandlePushSubscribeDeleteRemoves(t *testing.T) {
	isolatedHome(t)
	if err := addSubscription(webpush.Subscription{Endpoint: "https://push.example/d", Keys: webpush.Keys{Auth: "a", P256dh: "p"}}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handlePushSubscribe(w, httptest.NewRequest(http.MethodDelete, "/api/push-subscribe",
		strings.NewReader(`{"subscription":{"endpoint":"https://push.example/d"}}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", w.Code, w.Body)
	}
	if subs := loadSubscriptions(); len(subs) != 0 {
		t.Fatalf("delete left %+v", subs)
	}
}

func TestHandlePushSubscribeRejectsBadInput(t *testing.T) {
	isolatedHome(t)
	cases := map[string]string{
		"no endpoint":   `{"subscription":{"keys":{"auth":"a","p256dh":"p"}}}`,
		"no keys":       `{"subscription":{"endpoint":"https://push.example/x"}}`,
		"http endpoint": `{"subscription":{"endpoint":"http://push.example/x","keys":{"auth":"a","p256dh":"p"}}}`,
		"not json":      `nope`,
	}
	for name, body := range cases {
		w := httptest.NewRecorder()
		handlePushSubscribe(w, httptest.NewRequest(http.MethodPost, "/api/push-subscribe", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handlePushSubscribe(w, httptest.NewRequest(http.MethodGet, "/api/push-subscribe", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", w.Code)
	}
}

type fakeSend struct {
	calls  []fakeSendCall
	status map[string]int // endpoint -> status, default 201
}

type fakeSendCall struct {
	endpoint string
	message  []byte
	opts     *webpush.Options
}

func (f *fakeSend) send(_ context.Context, message []byte, s *webpush.Subscription, opts *webpush.Options) (*http.Response, error) {
	f.calls = append(f.calls, fakeSendCall{s.Endpoint, message, opts})
	code := f.status[s.Endpoint]
	if code == 0 {
		code = http.StatusCreated
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func useFakeSend(t *testing.T) *fakeSend {
	t.Helper()
	f := &fakeSend{status: map[string]int{}}
	prev := webpushSend
	webpushSend = f.send
	t.Cleanup(func() { webpushSend = prev })
	return f
}

func TestHandleNotifyFansOutToEverySubscription(t *testing.T) {
	isolatedHome(t)
	if _, err := loadOrCreateVAPID(); err != nil {
		t.Fatal(err)
	}
	f := useFakeSend(t)
	for _, ep := range []string{"https://push.example/1", "https://push.example/2"} {
		if err := addSubscription(webpush.Subscription{Endpoint: ep, Keys: webpush.Keys{Auth: "a", P256dh: "p"}}); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"bufferName":"Claude Agent @ home-lab","title":"home-lab","message":"Finished"}`
	w := httptest.NewRecorder()
	handleNotify(w, httptest.NewRequest(http.MethodPost, "/api/notify", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if len(f.calls) != 2 {
		t.Fatalf("sent %d pushes, want 2", len(f.calls))
	}
	var payload struct {
		Title      string `json:"title"`
		Body       string `json:"body"`
		BufferName string `json:"bufferName"`
		Tag        string `json:"tag"`
	}
	if err := json.Unmarshal(f.calls[0].message, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Title != "home-lab" || payload.Body != "Finished" ||
		payload.BufferName != "Claude Agent @ home-lab" || payload.Tag != "Claude Agent @ home-lab" {
		t.Fatalf("payload = %+v", payload)
	}
	opts := f.calls[0].opts
	if opts.VAPIDPublicKey == "" || opts.VAPIDPrivateKey == "" || opts.Subscriber == "" {
		t.Fatalf("VAPID options incomplete: %+v", opts)
	}
	if opts.TTL <= 0 {
		t.Fatalf("TTL must be positive, got %d", opts.TTL)
	}
	var resp struct {
		Sent int `json:"sent"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Sent != 2 {
		t.Fatalf("sent = %d, want 2: %s", resp.Sent, w.Body)
	}
}

func TestHandleNotifyDropsGoneSubscriptions(t *testing.T) {
	isolatedHome(t)
	if _, err := loadOrCreateVAPID(); err != nil {
		t.Fatal(err)
	}
	f := useFakeSend(t)
	f.status["https://push.example/gone"] = http.StatusGone
	f.status["https://push.example/missing"] = http.StatusNotFound
	for _, ep := range []string{"https://push.example/gone", "https://push.example/missing", "https://push.example/ok"} {
		if err := addSubscription(webpush.Subscription{Endpoint: ep, Keys: webpush.Keys{Auth: "a", P256dh: "p"}}); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	handleNotify(w, httptest.NewRequest(http.MethodPost, "/api/notify",
		strings.NewReader(`{"bufferName":"b","title":"t","message":"m"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	subs := loadSubscriptions()
	if len(subs) != 1 {
		t.Fatalf("want only the live subscription left, got %+v", subs)
	}
	if _, ok := subs["https://push.example/ok"]; !ok {
		t.Fatalf("live subscription was dropped: %+v", subs)
	}
}

func TestHandleNotifyWithoutSubscriptionsIsOK(t *testing.T) {
	isolatedHome(t)
	if _, err := loadOrCreateVAPID(); err != nil {
		t.Fatal(err)
	}
	f := useFakeSend(t)
	w := httptest.NewRecorder()
	handleNotify(w, httptest.NewRequest(http.MethodPost, "/api/notify",
		strings.NewReader(`{"bufferName":"b","title":"t","message":"m"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if len(f.calls) != 0 {
		t.Fatalf("sent %d pushes with no subscriptions", len(f.calls))
	}
}

func TestHandleNotifyRejectsBadInput(t *testing.T) {
	isolatedHome(t)
	f := useFakeSend(t)
	cases := map[string]string{
		"no buffer":  `{"title":"t","message":"m"}`,
		"bad buffer": `{"bufferName":"evil\"(kill-emacs)","title":"t","message":"m"}`,
		"no message": `{"bufferName":"b","title":"t"}`,
		"not json":   `nope`,
	}
	for name, body := range cases {
		w := httptest.NewRecorder()
		handleNotify(w, httptest.NewRequest(http.MethodPost, "/api/notify", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handleNotify(w, httptest.NewRequest(http.MethodGet, "/api/notify", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", w.Code)
	}
	if len(f.calls) != 0 {
		t.Fatalf("bad input still sent %d pushes", len(f.calls))
	}
}

func TestHandleNotifyTitleDefaultsToBufferName(t *testing.T) {
	isolatedHome(t)
	if _, err := loadOrCreateVAPID(); err != nil {
		t.Fatal(err)
	}
	f := useFakeSend(t)
	if err := addSubscription(webpush.Subscription{Endpoint: "https://push.example/1", Keys: webpush.Keys{Auth: "a", P256dh: "p"}}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handleNotify(w, httptest.NewRequest(http.MethodPost, "/api/notify",
		strings.NewReader(`{"bufferName":"Codex Agent @ x","message":"m"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(string(f.calls[0].message), `"title":"Codex Agent @ x"`) {
		t.Fatalf("title did not default: %s", f.calls[0].message)
	}
}

func TestServiceWorkerRouteServesUncachedRootScoped(t *testing.T) {
	w := httptest.NewRecorder()
	handleServiceWorker(w, httptest.NewRequest(http.MethodGet, "/sw.js", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	if swa := w.Header().Get("Service-Worker-Allowed"); swa != "/" {
		t.Fatalf("Service-Worker-Allowed = %q, want /", swa)
	}
	body := w.Body.String()
	for _, want := range []string{"addEventListener('push'", "addEventListener('notificationclick'", "showNotification", "open-session"} {
		if !strings.Contains(body, want) {
			t.Errorf("sw.js missing %q", want)
		}
	}
	if strings.Contains(body, "addEventListener('fetch'") {
		t.Error("sw.js must not install a fetch handler (no offline caching)")
	}
}

func TestManifestRouteIsStandalone(t *testing.T) {
	w := httptest.NewRecorder()
	handleManifest(w, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var m struct {
		Display  string `json:"display"`
		StartURL string `json:"start_url"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest not JSON: %v: %s", err, w.Body)
	}
	if m.Display != "standalone" || m.StartURL != "/" || m.Name == "" {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestIndexLinksManifestAndRegistersServiceWorker(t *testing.T) {
	html := string(indexHTML)
	if !strings.Contains(html, `rel="manifest"`) {
		t.Error("index.html must link the web app manifest")
	}
	if !strings.Contains(html, "serviceWorker.register(") {
		t.Error("index.html must register the service worker")
	}
}

func TestCSPAllowsSameOriginServiceWorker(t *testing.T) {
	csp := cspHeader("abc")
	if !strings.Contains(csp, "worker-src 'self'") {
		t.Fatalf("CSP must allow the service worker: %s", csp)
	}
	if !strings.Contains(csp, "script-src 'nonce-abc'") {
		t.Fatalf("CSP lost the script nonce: %s", csp)
	}
}

const serveStatusFixture = `{
  "TCP": {"443": {"HTTPS": true}},
  "Web": {
    "mrx.tail9179e0.ts.net:443": {
      "Handlers": {"/": {"Proxy": "http://127.0.0.1:8090"}}
    }
  }
}`

func TestServeURLFromStatusFindsHTTPSProxyForPort(t *testing.T) {
	if got := serveURLFromStatus([]byte(serveStatusFixture), "8090"); got != "https://mrx.tail9179e0.ts.net" {
		t.Fatalf("got %q", got)
	}
	if got := serveURLFromStatus([]byte(serveStatusFixture), "18091"); got != "" {
		t.Fatalf("other port must not match, got %q", got)
	}
	if got := serveURLFromStatus([]byte(`{}`), "8090"); got != "" {
		t.Fatalf("empty status must not match, got %q", got)
	}
	if got := serveURLFromStatus([]byte(`nope`), "8090"); got != "" {
		t.Fatalf("garbage must not match, got %q", got)
	}
}

func TestServeURLFromStatusKeepsNonDefaultHTTPSPort(t *testing.T) {
	status := `{"Web":{"mrx.tail9179e0.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8090"}}}}}`
	if got := serveURLFromStatus([]byte(status), "8090"); got != "https://mrx.tail9179e0.ts.net:8443" {
		t.Fatalf("got %q", got)
	}
}

func TestLinkURLPrefersHTTPSServe(t *testing.T) {
	if got := linkURL("https://mrx.tail9179e0.ts.net", "mrx.tail9179e0.ts.net", "8090", "K"); got != "https://mrx.tail9179e0.ts.net?authkey=K" {
		t.Fatalf("got %q", got)
	}
	if got := linkURL("", "mrx.tail9179e0.ts.net", "8090", "K"); got != "http://mrx.tail9179e0.ts.net:8090?authkey=K" {
		t.Fatalf("got %q", got)
	}
	if got := linkURL("", "", "8090", "K"); got != "http://127.0.0.1:8090?authkey=K" {
		t.Fatalf("got %q", got)
	}
}
