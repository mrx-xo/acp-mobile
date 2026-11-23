package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The phone never gets a generic eval endpoint: every HTTP handler pins
// one elisp function by name and validates its own inputs.  What they
// share is the mechanics of building the call, running emacsclient, and
// reading the reply, which live here so no handler hand-rolls quoting.
//
// Argument kinds:
//   elispStr  quoted elisp string with backslash and quote escaped; for
//             the legacy bridges (label, push, kill) that take raw strings
//   elispB64  quoted base64 of the bytes; preferred for new bridges, since
//             only base64's ASCII alphabet then crosses the boundary
//   elispRaw  literal token (t, nil, an integer)
//
// Reply convention: a bare nil means "no such thing" and maps to
// errElispNotFound; any other error is the daemon failing.

type elispArg struct{ text string }

func elispStr(s string) elispArg {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return elispArg{`"` + s + `"`}
}

func elispB64(s string) elispArg {
	return elispArg{`"` + base64.StdEncoding.EncodeToString([]byte(s)) + `"`}
}

func elispRaw(token string) elispArg { return elispArg{token} }

func elispBool(b bool) elispArg {
	if b {
		return elispRaw("t")
	}
	return elispRaw("nil")
}

func elispExpr(fn string, args ...elispArg) string {
	var sb strings.Builder
	sb.WriteString("(")
	sb.WriteString(fn)
	for _, a := range args {
		sb.WriteString(" ")
		sb.WriteString(a.text)
	}
	sb.WriteString(")")
	return sb.String()
}

var errElispNotFound = errors.New("elisp returned nil")

// elispError carries the daemon's output alongside the exec error so
// handlers can surface it to the phone.
type elispError struct {
	err    error
	output string
}

func (e *elispError) Error() string { return fmt.Sprintf("%v: %s", e.err, e.output) }
func (e *elispError) Unwrap() error { return e.err }

// wrapElispForFile wraps EXPR so Emacs writes the printed value to PATH
// and prints only t.  The write happens only while the file Go created
// still exists: Go removes it when the call returns, so an evaluation
// that finishes late (after cancellation) writes nothing.  PATH goes
// through filepath.ToSlash, which Emacs accepts on Windows.
func wrapElispForFile(expr, path string) string {
	lit := elispStr(filepath.ToSlash(path)).text
	return "(let ((v " + expr + ") (print-length nil) (print-level nil) (coding-system-for-write 'utf-8-unix))\n" +
		"  (when (file-exists-p " + lit + ")\n" +
		"    (write-region (prin1-to-string v) nil " + lit + " nil 'silent))\n" +
		"  t)"
}

// evalElispToFile evaluates EXPR in the daemon and returns exactly the
// text `emacsclient --eval EXPR` would print for the value (its prin1
// form).  The value travels through a temp file and emacsclient prints
// only t, because some emacsclient builds corrupt any printed reply
// longer than about 1 KB.  On a non-zero exit the returned bytes are
// emacsclient's combined output, not the file.
func evalElispToFile(ctx context.Context, expr string) ([]byte, error) {
	f, err := os.CreateTemp("", "acp-elisp-*")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	wrapped := wrapElispForFile(expr, path)
	out, err := evalEmacsContext(ctx, "emacsclient", "--eval", wrapped)
	if err != nil {
		return out, err
	}
	reply, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	if len(reply) == 0 {
		// prin1-to-string never yields an empty string, so an empty file
		// means Emacs did not write the reply.
		return out, fmt.Errorf("emacs wrote no reply: %s", strings.TrimSpace(string(out)))
	}
	return reply, nil
}

// callElisp evaluates FN with ARGS in the daemon and returns the trimmed
// printed result.  A bare nil result is errElispNotFound.
func callElisp(ctx context.Context, fn string, args ...elispArg) (string, error) {
	out, err := evalElispToFile(ctx, elispExpr(fn, args...))
	result := strings.TrimSpace(string(out))
	if err != nil {
		return result, &elispError{err: err, output: result}
	}
	if result == "nil" {
		return result, errElispNotFound
	}
	return result, nil
}

// writeElispError writes the JSON error reply for a failed callElisp:
// 404 with notFoundMsg on errElispNotFound, otherwise 500 with the
// daemon's output.  Returns true when it wrote a reply.
func writeElispError(w http.ResponseWriter, what string, err error, notFoundMsg string) bool {
	if err == nil {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	if errors.Is(err, errElispNotFound) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": notFoundMsg})
		return true
	}
	log.Printf("%s: %v", what, err)
	msg := err.Error()
	var ee *elispError
	if errors.As(err, &ee) {
		msg = ee.output
	}
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
	return true
}

// callElispJSON evaluates FN with ARGS and decodes the base64-armored
// JSON the newer bridges print (see syzygy-bridge.el).  A bare nil
// result is still errElispNotFound; garbled output is a daemon error.
func callElispJSON(ctx context.Context, fn string, args ...elispArg) ([]byte, error) {
	out, err := callElisp(ctx, fn, args...)
	if err != nil {
		return nil, err
	}
	decoded, err := unquoteElispBase64(out)
	if err != nil {
		return nil, &elispError{err: err, output: out}
	}
	return []byte(decoded), nil
}
