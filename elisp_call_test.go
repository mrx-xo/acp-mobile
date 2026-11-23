package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestElispExprQuotesEachArgumentKind(t *testing.T) {
	got := elispExpr("fn",
		elispStr(`a"b\c`),
		elispB64("héllo"),
		elispRaw("t"),
		elispRaw("42"))
	want := `(fn "a\"b\\c" "aMOpbGxv" t 42)`
	if got != want {
		t.Fatalf("elispExpr = %s, want %s", got, want)
	}
}

func TestCallElispMapsNilToNotFound(t *testing.T) {
	installFakeEmacsclient(t, "nil")
	_, err := callElisp(context.Background(), "fn", elispStr("x"))
	if !errors.Is(err, errElispNotFound) {
		t.Fatalf("err = %v, want errElispNotFound", err)
	}
}

func TestCallElispReturnsTrimmedOutput(t *testing.T) {
	argsFile := installFakeEmacsclient(t, `"on"`)
	out, err := callElisp(context.Background(), "fn", elispStr("x"))
	if err != nil {
		t.Fatal(err)
	}
	if out != `"on"` {
		t.Fatalf("out = %q, want %q", out, `"on"`)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), `(fn "x")`) {
		t.Fatalf("emacsclient args = %q", args)
	}
}

func postJSON(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestLabelHandlerSendsEscapedExpression(t *testing.T) {
	argsFile := installFakeEmacsclient(t, `"ok"`)
	rec := postJSON(t, handleLabel, `{"bufferName":"Claude Agent @ x<2>","label":"say \"hi\""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	args, _ := os.ReadFile(argsFile)
	want := `(mr-x/agent-label-set "Claude Agent @ x<2>" "say \"hi\"")`
	if !strings.Contains(string(args), want) {
		t.Fatalf("emacsclient args = %q, want %q", args, want)
	}
}

func TestLabelHandlerNilIsNotFound(t *testing.T) {
	installFakeEmacsclient(t, "nil")
	rec := postJSON(t, handleLabel, `{"bufferName":"Claude Agent @ x","label":"l"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestPushHandlerReportsState(t *testing.T) {
	argsFile := installFakeEmacsclient(t, `"on"`)
	rec := postJSON(t, handlePush, `{"bufferName":"Claude Agent @ x","enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK   bool `json:"ok"`
		Push bool `json:"push"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !resp.Push {
		t.Fatalf("resp = %+v", resp)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), `(agent-shell-push-set "Claude Agent @ x" t)`) {
		t.Fatalf("emacsclient args = %q", args)
	}
}

func TestKillHandlerNilIsNotFound(t *testing.T) {
	argsFile := installFakeEmacsclient(t, "nil")
	rec := postJSON(t, handleKill, `{"bufferName":"Claude Agent @ x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), `(meta-agent-shell-close-session "Claude Agent @ x")`) {
		t.Fatalf("emacsclient args = %q", args)
	}
}

// installFileWritingEmacsclient shadows emacsclient with a script that
// does what a real Emacs does with the wrapped expression: write the
// reply to the path named in the write-region call and print only t.
// FAKE_REPLY_FILE names a file holding the reply; the args are recorded.
func installFileWritingEmacsclient(t *testing.T, reply string, exitCode int) (argsFile string, pathFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	pathFile = filepath.Join(dir, "target")
	replyFile := filepath.Join(dir, "reply")
	if err := os.WriteFile(replyFile, []byte(reply), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$FAKE_EMACSCLIENT_ARGS\"\n" +
		"p=$(printf '%s' \"$2\" | sed -n 's/.*(prin1-to-string v) nil \"\\([^\"]*\\)\".*/\\1/p')\n" +
		"printf '%s' \"$p\" > \"$FAKE_TARGET_FILE\"\n" +
		"if [ \"$FAKE_EXIT\" != 0 ]; then echo 'emacsclient: boom'; exit \"$FAKE_EXIT\"; fi\n" +
		"cp \"$FAKE_REPLY_FILE\" \"$p\"\n" +
		"echo t\n"
	if err := os.WriteFile(filepath.Join(dir, "emacsclient"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_EMACSCLIENT_ARGS", argsFile)
	t.Setenv("FAKE_TARGET_FILE", pathFile)
	t.Setenv("FAKE_REPLY_FILE", replyFile)
	t.Setenv("FAKE_EXIT", strconv.Itoa(exitCode))
	previousConfig := appConfig
	appConfig = config{}
	t.Cleanup(func() { appConfig = previousConfig })
	return argsFile, pathFile
}

func TestEvalElispToFileReturnsALongReplyWhole(t *testing.T) {
	reply := `"` + strings.Repeat("a", 19998) + `"`
	installFileWritingEmacsclient(t, reply, 0)
	out, err := evalElispToFile(context.Background(), "(big)")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != reply || len(out) != 20000 {
		t.Fatalf("got %d bytes, want the %d-byte reply byte-equal", len(out), len(reply))
	}
}

func TestEvalElispToFileWrapsTheExpression(t *testing.T) {
	argsFile, pathFile := installFileWritingEmacsclient(t, `"x"`, 0)
	if _, err := evalElispToFile(context.Background(), "(fn 1)"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(argsFile)
	got, ok := strings.CutPrefix(strings.TrimRight(string(raw), "\n"), "--eval\n")
	if !ok {
		t.Fatalf("args = %q", raw)
	}
	target, _ := os.ReadFile(pathFile)
	want := "(let ((v (fn 1)) (print-length nil) (print-level nil) (coding-system-for-write 'utf-8-unix))\n" +
		"  (write-region (prin1-to-string v) nil " + elispStr(string(target)).text + " nil 'silent)\n" +
		"  t)"
	if got != want {
		t.Fatalf("expression = %q, want %q", got, want)
	}
	if strings.Contains(string(target), `\`) {
		t.Fatalf("path %q has a backslash", target)
	}
}

func TestElispPathLiteralUsesForwardSlashes(t *testing.T) {
	got := elispPathLiteral(`C:\Users\x\AppData\Local\Temp\acp-elisp-123`)
	want := `"C:/Users/x/AppData/Local/Temp/acp-elisp-123"`
	if got != want {
		t.Fatalf("elispPathLiteral = %s, want %s", got, want)
	}
}

func TestEvalElispToFileKeepsNilAndErrors(t *testing.T) {
	installFileWritingEmacsclient(t, "nil", 0)
	_, err := callElisp(context.Background(), "fn")
	if !errors.Is(err, errElispNotFound) {
		t.Fatalf("err = %v, want errElispNotFound", err)
	}

	installFileWritingEmacsclient(t, "ignored", 3)
	out, err := evalElispToFile(context.Background(), "(fn)")
	if err == nil {
		t.Fatal("want an error for a non-zero exit")
	}
	if !strings.Contains(string(out), "emacsclient: boom") {
		t.Fatalf("output = %q, want emacsclient's combined output", out)
	}
	_, err = callElisp(context.Background(), "fn")
	var ee *elispError
	if !errors.As(err, &ee) || ee.output != "emacsclient: boom" {
		t.Fatalf("err = %v, want elispError carrying the output", err)
	}
}

func TestEvalElispToFileRemovesItsFile(t *testing.T) {
	for _, exit := range []int{0, 3} {
		_, pathFile := installFileWritingEmacsclient(t, `"x"`, exit)
		evalElispToFile(context.Background(), "(fn)")
		target, _ := os.ReadFile(pathFile)
		if len(target) == 0 {
			t.Fatalf("exit %d: fake never saw a path", exit)
		}
		if _, err := os.Stat(string(target)); !os.IsNotExist(err) {
			t.Fatalf("exit %d: temp file still present (stat err = %v)", exit, err)
		}
	}
}

func TestCallElispUsesTheFilePath(t *testing.T) {
	payload := strings.Repeat("0123456789", 1500)
	installFileWritingEmacsclient(t, encodedElispJSON(payload), 0)
	got, err := callElispJSON(context.Background(), "fn")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("decoded %d bytes, want %d", len(got), len(payload))
	}
}
