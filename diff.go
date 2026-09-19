package main

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Diff review = a read-only, phone-first view of what changed. Two
// scopes: the latest turn this phone started (an immutable snapshot
// captured around the prompt) and the repository's current staged,
// unstaged and untracked changes. Nothing here writes to the working
// tree, the index, or the refs.

// diffOutputLimit bounds every git diff read. Past it the response is
// marked truncated and the tail is dropped on a line boundary.
const diffOutputLimit = 2 << 20

type reviewLine struct {
	Kind string `json:"kind"` // context, added, removed, marker
	Old  int    `json:"old,omitempty"`
	New  int    `json:"new,omitempty"`
	Text string `json:"text"`
}

type reviewHunk struct {
	Header string       `json:"header"`
	Lines  []reviewLine `json:"lines"`
}

type reviewFile struct {
	Path    string       `json:"path"`
	OldPath string       `json:"oldPath,omitempty"`
	Status  string       `json:"status"` // added, deleted, renamed, modified
	Source  string       `json:"source"` // turn, staged, unstaged, untracked
	Added   int          `json:"added"`
	Removed int          `json:"removed"`
	Binary  bool         `json:"binary,omitempty"`
	Hunks   []reviewHunk `json:"hunks,omitempty"`
}

type reviewResult struct {
	Scope      string       `json:"scope"`
	Repository string       `json:"repository"`
	Branch     string       `json:"branch"`
	CapturedAt time.Time    `json:"capturedAt"`
	Available  bool         `json:"available"`
	Pending    bool         `json:"pending,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	Truncated  bool         `json:"truncated,omitempty"`
	Added      int          `json:"added"`
	Removed    int          `json:"removed"`
	Files      []reviewFile `json:"files"`
}

// parseUnifiedDiff turns `git diff --no-color --no-ext-diff --unified=3`
// output into files, hunks and numbered lines. Old and new counters run
// independently: a removal consumes only an old number, an addition only
// a new one, context both. The no-newline record is a marker that
// consumes neither. A file whose header was cut off by the output limit
// is dropped; a hunk cut mid-way keeps the lines it has.
func parseUnifiedDiff(raw []byte, source string) ([]reviewFile, error) {
	var files []reviewFile
	var cur *reviewFile
	var hunk *reviewHunk
	var oldLine, newLine int

	flushHunk := func() {
		if cur != nil && hunk != nil {
			cur.Hunks = append(cur.Hunks, *hunk)
		}
		hunk = nil
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
		}
		cur = nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), diffOutputLimit+1)
	complete := bytes.HasSuffix(raw, []byte("\n"))
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !complete && len(lines) > 0 {
		// The last line was cut by the output limit: never trust it.
		lines = lines[:len(lines)-1]
	}

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flushFile()
			cur = &reviewFile{Status: "modified", Source: source}
			cur.Path = diffGitPath(strings.TrimPrefix(line, "diff --git "))
			oldLine, newLine = 0, 0
		case cur == nil:
			// Preamble noise (warnings) before the first file.
		case hunk != nil && len(line) > 0 && (line[0] == ' ' || line[0] == '+' || line[0] == '-' || line[0] == '\\'):
			text := line[1:]
			switch line[0] {
			case ' ':
				hunk.Lines = append(hunk.Lines, reviewLine{Kind: "context", Old: oldLine, New: newLine, Text: text})
				oldLine++
				newLine++
			case '+':
				hunk.Lines = append(hunk.Lines, reviewLine{Kind: "added", New: newLine, Text: text})
				newLine++
				cur.Added++
			case '-':
				hunk.Lines = append(hunk.Lines, reviewLine{Kind: "removed", Old: oldLine, Text: text})
				oldLine++
				cur.Removed++
			case '\\':
				hunk.Lines = append(hunk.Lines, reviewLine{Kind: "marker", Text: strings.TrimSpace(text)})
			}
		case hunk != nil && line == "":
			// Some producers drop the leading space of a blank context line.
			hunk.Lines = append(hunk.Lines, reviewLine{Kind: "context", Old: oldLine, New: newLine, Text: ""})
			oldLine++
			newLine++
		case strings.HasPrefix(line, "@@ "):
			flushHunk()
			o, n, ok := parseHunkHeader(line)
			if !ok {
				continue
			}
			oldLine, newLine = o, n
			hunk = &reviewHunk{Header: line, Lines: []reviewLine{}}
		case strings.HasPrefix(line, "--- "):
			if p, ok := diffPathAfterPrefix(line[4:], "a/"); ok {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ "):
			if p, ok := diffPathAfterPrefix(line[4:], "b/"); ok {
				cur.Path = p
			}
		case strings.HasPrefix(line, "rename from "):
			cur.OldPath = unquoteDiffPath(strings.TrimPrefix(line, "rename from "))
			cur.Status = "renamed"
		case strings.HasPrefix(line, "rename to "):
			cur.Path = unquoteDiffPath(strings.TrimPrefix(line, "rename to "))
			cur.Status = "renamed"
		case strings.HasPrefix(line, "copy from "):
			cur.OldPath = unquoteDiffPath(strings.TrimPrefix(line, "copy from "))
			cur.Status = "added"
		case strings.HasPrefix(line, "copy to "):
			cur.Path = unquoteDiffPath(strings.TrimPrefix(line, "copy to "))
		case strings.HasPrefix(line, "new file mode "):
			cur.Status = "added"
		case strings.HasPrefix(line, "deleted file mode "):
			cur.Status = "deleted"
		case strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch":
			cur.Binary = true
		}
	}
	flushFile()

	for i := range files {
		f := &files[i]
		if f.Status != "renamed" && f.OldPath == f.Path {
			f.OldPath = ""
		}
		if f.Status == "deleted" && f.Path == "" {
			f.Path = f.OldPath
		}
		if f.Status != "renamed" {
			f.OldPath = ""
		}
	}
	return files, nil
}

// diffGitPath recovers a path from the `a/X b/Y` tail of a diff --git
// line when X == Y (the common case). Renames overwrite it from their
// own headers, so an ambiguous split here is harmless.
func diffGitPath(s string) string {
	if strings.HasPrefix(s, "\"") {
		// "a/quoted" "b/quoted"
		if end := strings.Index(s[1:], "\" \""); end >= 0 {
			return strings.TrimPrefix(unquoteDiffPath(s[:end+2]), "a/")
		}
		return ""
	}
	// len = 2*len(path) + len("a/") + len(" b/")
	if (len(s)-5)%2 != 0 {
		return ""
	}
	half := (len(s) - 5) / 2
	if !strings.HasPrefix(s, "a/") || s[2+half:5+half] != " b/" || s[2:2+half] != s[5+half:] {
		return ""
	}
	return s[2 : 2+half]
}

// diffPathAfterPrefix reads the path on a ---/+++ line. Git appends a
// tab when the name holds a space; /dev/null means no such side.
func diffPathAfterPrefix(s, prefix string) (string, bool) {
	s = strings.TrimRight(s, "\t")
	if s == "/dev/null" {
		return "", false
	}
	s = unquoteDiffPath(s)
	return strings.TrimPrefix(s, prefix), true
}

// unquoteDiffPath undoes git's C-style quoting of unusual path bytes.
func unquoteDiffPath(s string) string {
	s = strings.TrimRight(s, "\t")
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if out, err := strconv.Unquote(s); err == nil {
			return out
		}
	}
	return s
}

// parseHunkHeader reads the starting old and new line numbers from an
// `@@ -old[,count] +new[,count] @@` header. A zero-count side (a pure
// addition or deletion) still reports its start.
func parseHunkHeader(line string) (int, int, bool) {
	rest := strings.TrimPrefix(line, "@@ ")
	end := strings.Index(rest, " @@")
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(rest[:end])
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return 0, 0, false
	}
	start := func(spec string) (int, bool) {
		spec = spec[1:]
		if i := strings.IndexByte(spec, ','); i >= 0 {
			spec = spec[:i]
		}
		n, err := strconv.Atoi(spec)
		return n, err == nil
	}
	o, ok1 := start(fields[0])
	n, ok2 := start(fields[1])
	return o, n, ok1 && ok2
}

// boundedRead reads at most limit bytes. When the source holds more, it
// reports truncation and cuts the output back to the last complete line
// so no partial line and no partial code point survives.
func boundedRead(r io.Reader, limit int) ([]byte, bool, error) {
	buf, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(buf) <= limit {
		return buf, false, nil
	}
	buf = buf[:limit]
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[:i+1]
	} else {
		for len(buf) > 0 && !utf8.Valid(buf) {
			buf = buf[:len(buf)-1]
		}
	}
	return buf, true, nil
}
