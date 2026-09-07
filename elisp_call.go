package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
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

// callElisp evaluates FN with ARGS in the daemon and returns the trimmed
// printed result.  A bare nil result is errElispNotFound.
func callElisp(ctx context.Context, fn string, args ...elispArg) (string, error) {
	out, err := evalEmacsContext(ctx, "emacsclient", "--eval", elispExpr(fn, args...))
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
