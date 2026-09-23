package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// --- Read-only git access ---

// sessionCwd resolves a live session's working directory. A var so tests
// can point a fake session at a temporary repository.
var sessionCwd = processCwd

// gitReadOnlyEnv keeps every git call from taking optional locks (index
// refresh) and from reading user diff drivers or pagers.
var gitReadOnlyEnv = []string{"GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "LC_ALL=C"}

// gitDiffArgs is the fixed shape of every patch this feature reads.
var gitDiffArgs = []string{"--no-color", "--no-ext-diff", "--no-textconv", "--unified=3", "--find-renames"}

func gitCommand(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	full := append([]string{
		"-c", "core.quotePath=false", "-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false",
		"-c", "diff.relative=false", "-c", "core.fsmonitor=false",
	}, args...)
	cmd := commandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), gitReadOnlyEnv...), env...)
	return cmd
}

// runGit returns trimmed stdout of a small git query.
func runGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := gitCommand(ctx, dir, env, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// runGitBounded streams a patch-producing git command through
// boundedRead, killing git once the limit is reached. Exit status 1 is
// accepted because `git diff --no-index` uses it to mean "differs".
func runGitBounded(ctx context.Context, dir string, env []string, limit int, args ...string) ([]byte, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := gitCommand(ctx, dir, env, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	out, truncated, readErr := boundedRead(stdout, limit)
	if truncated {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, false, readErr
	}
	if waitErr != nil && !truncated {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) || exit.ExitCode() != 1 {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = waitErr.Error()
			}
			return nil, false, fmt.Errorf("git %s: %s", args[0], msg)
		}
	}
	return out, truncated, nil
}

// repoInfo is what every review needs to know about a repository.
type repoInfo struct {
	Root    string
	Branch  string
	HasHead bool
	Objects string
}

func inspectRepository(ctx context.Context, dir string) (repoInfo, error) {
	root, err := runGit(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return repoInfo{}, err
	}
	info := repoInfo{Root: root}
	if _, err := runGit(ctx, root, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err == nil {
		info.HasHead = true
	}
	if branch, err := runGit(ctx, root, nil, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		info.Branch = branch
	} else if short, err := runGit(ctx, root, nil, "rev-parse", "--short", "HEAD"); err == nil {
		info.Branch = "detached at " + short
	}
	if objects, err := runGit(ctx, root, nil, "rev-parse", "--path-format=absolute", "--git-path", "objects"); err == nil {
		info.Objects = objects
	}
	return info, nil
}

// gitStatusResult is the header's view of a repository: the branch and
// how many paths are staged, unstaged and untracked. A path that is
// both staged and unstaged counts once in each.
type gitStatusResult struct {
	Branch    string `json:"branch"`
	Staged    int    `json:"staged"`
	Unstaged  int    `json:"unstaged"`
	Untracked int    `json:"untracked"`
	Truncated bool   `json:"truncated,omitempty"`
}

// parsePorcelainStatus counts entries of `git status --porcelain=v1 -z`.
// Entries are "XY path" separated by NUL; a rename or copy carries the
// original path in the following NUL field, which is skipped.
func parsePorcelainStatus(raw []byte) gitStatusResult {
	var res gitStatusResult
	fields := bytes.Split(raw, []byte{0})
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 3 {
			continue
		}
		x, y := entry[0], entry[1]
		if x == '?' && y == '?' {
			res.Untracked++
			continue
		}
		if x == '!' && y == '!' {
			continue
		}
		if x != ' ' {
			res.Staged++
		}
		if y != ' ' {
			res.Unstaged++
		}
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++ // the original path travels in the next field
		}
	}
	return res
}

// gitStatus reads the branch and counts for dir. A directory that is
// not a repository yields an empty branch and zero counts, not an error.
func gitStatus(ctx context.Context, dir string) (gitStatusResult, error) {
	info, err := inspectRepository(ctx, dir)
	if err != nil {
		return gitStatusResult{}, nil
	}
	raw, truncated, err := runGitBounded(ctx, info.Root, nil, diffOutputLimit, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return gitStatusResult{}, err
	}
	res := parsePorcelainStatus(raw)
	res.Branch = info.Branch
	res.Truncated = truncated
	return res, nil
}

// handleGitStatus serves GET /api/git-status?pid=PID for a live session.
func handleGitStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pid, err := strconv.Atoi(r.URL.Query().Get("pid"))
	if err != nil || pid <= 0 {
		http.Error(w, "invalid pid", http.StatusBadRequest)
		return
	}
	if findSocket(strconv.Itoa(pid)) == "" {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var res gitStatusResult
	if dir := sessionCwd(pid); dir != "" {
		if res, err = gitStatus(ctx, dir); err != nil {
			log.Printf("git-status: %v", err)
			http.Error(w, "status failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// baseTree is what the index is compared against: HEAD, or the empty
// tree in a repository that has no commit yet.
func baseTree(ctx context.Context, info repoInfo) (string, error) {
	if info.HasHead {
		return "HEAD", nil
	}
	return runGit(ctx, info.Root, nil, "hash-object", "-t", "tree", "/dev/null")
}

func finishReview(res *reviewResult, noChanges string) {
	if res.Files == nil {
		res.Files = []reviewFile{}
	}
	res.Added, res.Removed = 0, 0
	for _, f := range res.Files {
		res.Added += f.Added
		res.Removed += f.Removed
	}
	if len(res.Files) == 0 && res.Reason == "" {
		res.Reason = noChanges
	}
}

// untrackedPatchLimit caps how many untracked files get their own git
// process; past it the response is marked truncated.
const untrackedPatchLimit = 200

// reviewDirectory computes the repository scope for dir: staged against
// HEAD, unstaged against the index, untracked as additions from
// /dev/null. It never writes to the repository.
func reviewDirectory(ctx context.Context, dir string) (reviewResult, error) {
	res := reviewResult{Scope: "repository", CapturedAt: time.Now()}
	info, err := inspectRepository(ctx, dir)
	if err != nil {
		res.Reason = "Not a Git repository"
		finishReview(&res, res.Reason)
		return res, nil
	}
	res.Repository, res.Branch, res.Available = info.Root, info.Branch, true
	base, err := baseTree(ctx, info)
	if err != nil {
		return res, err
	}
	budget := diffOutputLimit
	patch := func(source string, args ...string) error {
		if budget <= 0 {
			res.Truncated = true
			return nil
		}
		out, truncated, err := runGitBounded(ctx, info.Root, nil, budget, args...)
		if err != nil {
			return err
		}
		budget -= len(out)
		if truncated {
			res.Truncated = true
		}
		files, err := parseUnifiedDiff(out, source)
		if err != nil {
			return err
		}
		res.Files = append(res.Files, files...)
		return nil
	}
	staged := append(append([]string{"diff", "--cached"}, gitDiffArgs...), base)
	if err := patch("staged", staged...); err != nil {
		return res, err
	}
	if err := patch("unstaged", append([]string{"diff"}, gitDiffArgs...)...); err != nil {
		return res, err
	}
	listed, err := runGit(ctx, info.Root, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return res, err
	}
	for i, path := range strings.Split(listed, "\x00") {
		if path == "" {
			continue
		}
		if i >= untrackedPatchLimit {
			res.Truncated = true
			break
		}
		args := append(append([]string{"diff", "--no-index"}, gitDiffArgs...), "--", "/dev/null", path)
		if err := patch("untracked", args...); err != nil {
			return res, err
		}
	}
	finishReview(&res, "No changes to review")
	return res, nil
}

// pidFromSocketPath reads the multiplex pid out of either socket layout.
func pidFromSocketPath(sockPath string) (int, error) {
	name := strings.TrimSuffix(filepath.Base(sockPath), ".sock")
	name = strings.TrimPrefix(name, "acp-multiplex-")
	pid, err := strconv.Atoi(name)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("no pid in socket path %q", sockPath)
	}
	return pid, nil
}

// repositoryReview resolves pid through the live socket catalogue, so an
// arbitrary path can never be reviewed.
func repositoryReview(ctx context.Context, pid int) (reviewResult, error) {
	if pid <= 0 || findSocket(strconv.Itoa(pid)) == "" {
		return reviewResult{}, fmt.Errorf("no live session for pid %d", pid)
	}
	dir := sessionCwd(pid)
	if dir == "" {
		res := reviewResult{Scope: "repository", CapturedAt: time.Now(), Reason: "Working directory unknown"}
		finishReview(&res, res.Reason)
		return res, nil
	}
	return reviewDirectory(ctx, dir)
}

// --- Turn snapshots ---

// A turn snapshot diffs the working tree before a phone prompt against
// the tree after its response. Both trees are written into a temporary
// object directory (the real one is only an alternate for reads) through
// a temporary index, so the capture leaves the repository's own index
// and objects untouched. The completed result is saved per session and
// stays immutable until the next completed turn replaces it.

type turnBaseline struct {
	SessionID string
	Root      string
	TempDir   string
	Tree      string
	StartedAt time.Time
	info      repoInfo
	env       []string
}

var turnCaptures = struct {
	mu sync.Mutex
	m  map[string]int // sessionId -> captures in progress
}{m: map[string]int{}}

func captureDir() string  { return filepath.Join(acpMobileDir(), "diff-captures") }
func turnDiffDir() string { return filepath.Join(acpMobileDir(), "turn-diffs") }

func turnDiffPath(sessionID string) string {
	return filepath.Join(turnDiffDir(), sessionID+".json")
}

// cleanupTurnCaptures sweeps captures a previous process left behind. A
// baseline without its completion is worthless, and must never become a
// snapshot.
func cleanupTurnCaptures() {
	os.RemoveAll(captureDir())
}

// snapshotTree stages the whole working tree into the temporary index
// and returns the resulting tree id.
func snapshotTree(ctx context.Context, b *turnBaseline) (string, error) {
	if _, err := runGit(ctx, b.Root, b.env, "add", "-A", "--ignore-errors"); err != nil {
		return "", err
	}
	return runGit(ctx, b.Root, b.env, "write-tree")
}

func beginTurnReview(ctx context.Context, sessionID, socketPath string) (*turnBaseline, error) {
	if !validSessionID.MatchString(sessionID) {
		return nil, fmt.Errorf("invalid session id")
	}
	pid, err := pidFromSocketPath(socketPath)
	if err != nil {
		return nil, err
	}
	dir := sessionCwd(pid)
	if dir == "" {
		return nil, fmt.Errorf("working directory unknown for pid %d", pid)
	}
	info, err := inspectRepository(ctx, dir)
	if err != nil {
		return nil, err
	}
	if info.Objects == "" {
		return nil, fmt.Errorf("object directory unknown for %s", info.Root)
	}
	if err := os.MkdirAll(captureDir(), 0700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(captureDir(), "turn-")
	if err != nil {
		return nil, err
	}
	b := &turnBaseline{SessionID: sessionID, Root: info.Root, TempDir: tmp, StartedAt: time.Now(), info: info}
	if err := os.Mkdir(filepath.Join(tmp, "objects"), 0700); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	b.env = []string{
		"GIT_INDEX_FILE=" + filepath.Join(tmp, "index"),
		"GIT_OBJECT_DIRECTORY=" + filepath.Join(tmp, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + info.Objects,
	}
	readTree := []string{"read-tree", "--empty"}
	if info.HasHead {
		readTree = []string{"read-tree", "HEAD"}
	}
	if _, err := runGit(ctx, b.Root, b.env, readTree...); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	tree, err := snapshotTree(ctx, b)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	b.Tree = tree
	turnCaptures.mu.Lock()
	turnCaptures.m[sessionID]++
	turnCaptures.mu.Unlock()
	return b, nil
}

func releaseTurnCapture(b *turnBaseline) {
	os.RemoveAll(b.TempDir)
	turnCaptures.mu.Lock()
	defer turnCaptures.mu.Unlock()
	if turnCaptures.m[b.SessionID] <= 1 {
		delete(turnCaptures.m, b.SessionID)
	} else {
		turnCaptures.m[b.SessionID]--
	}
}

func abortTurnReview(b *turnBaseline) {
	if b == nil {
		return
	}
	releaseTurnCapture(b)
}

func completeTurnReview(ctx context.Context, b *turnBaseline) (reviewResult, error) {
	if b == nil {
		return reviewResult{}, fmt.Errorf("no baseline")
	}
	defer releaseTurnCapture(b)
	res := reviewResult{Scope: "turn", Repository: b.Root, CapturedAt: time.Now(), Available: true}
	if info, err := inspectRepository(ctx, b.Root); err == nil {
		res.Branch = info.Branch
	}
	after, err := snapshotTree(ctx, b)
	if err != nil {
		return res, err
	}
	args := append([]string{"diff-tree", "-r", "-p"}, gitDiffArgs...)
	args = append(args, b.Tree, after)
	out, truncated, err := runGitBounded(ctx, b.Root, b.env, diffOutputLimit, args...)
	if err != nil {
		return res, err
	}
	res.Truncated = truncated
	if res.Files, err = parseUnifiedDiff(out, "turn"); err != nil {
		return res, err
	}
	finishReview(&res, "No changes in this turn")
	if err := saveTurnReview(res, b.SessionID); err != nil {
		return res, err
	}
	return res, nil
}

// saveTurnReview writes the snapshot atomically: a reader never sees a
// half-written file, and a failed write leaves the previous one intact.
func saveTurnReview(res reviewResult, sessionID string) error {
	if err := os.MkdirAll(turnDiffDir(), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(turnDiffDir(), sessionID+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), turnDiffPath(sessionID))
}

// loadTurnReview reports a capture in progress, else the saved snapshot,
// else an explicit unavailable state. The repository diff is never
// substituted for a missing snapshot.
func loadTurnReview(sessionID string) reviewResult {
	turnCaptures.mu.Lock()
	pending := turnCaptures.m[sessionID] > 0
	turnCaptures.mu.Unlock()
	if pending {
		return reviewResult{Scope: "turn", Pending: true, Reason: "Capturing this turn", Files: []reviewFile{}}
	}
	data, err := os.ReadFile(turnDiffPath(sessionID))
	if err == nil {
		var res reviewResult
		if json.Unmarshal(data, &res) == nil && res.Available {
			if res.Files == nil {
				res.Files = []reviewFile{}
			}
			return res
		}
	}
	return reviewResult{Scope: "turn", Reason: "Snapshot unavailable", Files: []reviewFile{}}
}

// --- HTTP ---

// handleDiffReview serves GET /api/diff-review:
//
//	?scope=turn&sessionId=ID   the latest saved snapshot, pending, or unavailable
//	?scope=repository&pid=PID  a fresh staged/unstaged/untracked review
func handleDiffReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	var res reviewResult
	switch q.Get("scope") {
	case "turn":
		sid := q.Get("sessionId")
		if !validSessionID.MatchString(sid) {
			http.Error(w, "invalid session id", http.StatusBadRequest)
			return
		}
		res = loadTurnReview(sid)
	case "repository":
		pid, err := strconv.Atoi(q.Get("pid"))
		if err != nil || pid <= 0 {
			http.Error(w, "invalid pid", http.StatusBadRequest)
			return
		}
		if findSocket(strconv.Itoa(pid)) == "" {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		res, err = repositoryReview(ctx, pid)
		if err != nil {
			log.Printf("diff-review: %v", err)
			http.Error(w, "review failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "scope must be turn or repository", http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}
