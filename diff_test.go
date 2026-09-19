package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/websocket"
)

func TestParseUnifiedDiff(t *testing.T) {
	ctx := func(old, new int, text string) reviewLine {
		return reviewLine{Kind: "context", Old: old, New: new, Text: text}
	}
	add := func(new int, text string) reviewLine { return reviewLine{Kind: "added", New: new, Text: text} }
	del := func(old int, text string) reviewLine { return reviewLine{Kind: "removed", Old: old, Text: text} }
	marker := reviewLine{Kind: "marker", Text: "No newline at end of file"}

	cases := []struct {
		name string
		raw  string
		want []reviewFile
	}{
		{
			name: "modified file numbers old and new independently",
			raw: "diff --git a/main.go b/main.go\n" +
				"index 1111111..2222222 100644\n" +
				"--- a/main.go\n" +
				"+++ b/main.go\n" +
				"@@ -1,4 +1,5 @@\n" +
				" package main\n" +
				" \n" +
				"-import \"fmt\"\n" +
				"+import \"os\"\n" +
				"+import \"fmt\"\n" +
				" \n",
			want: []reviewFile{{
				Path: "main.go", Status: "modified", Source: "staged", Added: 2, Removed: 1,
				Hunks: []reviewHunk{{Header: "@@ -1,4 +1,5 @@", Lines: []reviewLine{
					ctx(1, 1, "package main"), ctx(2, 2, ""), del(3, `import "fmt"`),
					add(3, `import "os"`), add(4, `import "fmt"`), ctx(4, 5, ""),
				}}},
			}},
		},
		{
			name: "rename with spaces keeps both paths and hunk numbering",
			raw: "diff --git a/docs/old name.md b/docs/new name.md\n" +
				"similarity index 90%\n" +
				"rename from docs/old name.md\n" +
				"rename to docs/new name.md\n" +
				"index 3333333..4444444 100644\n" +
				"--- a/docs/old name.md\t\n" +
				"+++ b/docs/new name.md\t\n" +
				"@@ -10,3 +10,3 @@ ## Heading\n" +
				" one\n" +
				"-two\n" +
				"+deux\n" +
				" three\n",
			want: []reviewFile{{
				Path: "docs/new name.md", OldPath: "docs/old name.md", Status: "renamed", Source: "staged",
				Added: 1, Removed: 1,
				Hunks: []reviewHunk{{Header: "@@ -10,3 +10,3 @@ ## Heading", Lines: []reviewLine{
					ctx(10, 10, "one"), del(11, "two"), add(11, "deux"), ctx(12, 12, "three"),
				}}},
			}},
		},
		{
			name: "pure rename has no hunks",
			raw: "diff --git a/a.txt b/b.txt\n" +
				"similarity index 100%\n" +
				"rename from a.txt\n" +
				"rename to b.txt\n",
			want: []reviewFile{{Path: "b.txt", OldPath: "a.txt", Status: "renamed", Source: "staged"}},
		},
		{
			name: "deletion uses old numbers only",
			raw: "diff --git a/gone.txt b/gone.txt\n" +
				"deleted file mode 100644\n" +
				"index 5555555..0000000\n" +
				"--- a/gone.txt\n" +
				"+++ /dev/null\n" +
				"@@ -1,2 +0,0 @@\n" +
				"-first\n" +
				"-second\n",
			want: []reviewFile{{
				Path: "gone.txt", Status: "deleted", Source: "unstaged", Removed: 2,
				Hunks: []reviewHunk{{Header: "@@ -1,2 +0,0 @@", Lines: []reviewLine{del(1, "first"), del(2, "second")}}},
			}},
		},
		{
			name: "new file uses new numbers only",
			raw: "diff --git a/new.txt b/new.txt\n" +
				"new file mode 100644\n" +
				"index 0000000..9999999\n" +
				"--- /dev/null\n" +
				"+++ b/new.txt\n" +
				"@@ -0,0 +1,2 @@\n" +
				"+a\n" +
				"+b\n",
			want: []reviewFile{{
				Path: "new.txt", Status: "added", Source: "untracked", Added: 2,
				Hunks: []reviewHunk{{Header: "@@ -0,0 +1,2 @@", Lines: []reviewLine{add(1, "a"), add(2, "b")}}},
			}},
		},
		{
			name: "missing final newline becomes a marker that consumes no numbers",
			raw: "diff --git a/end.txt b/end.txt\n" +
				"index 6666666..7777777 100644\n" +
				"--- a/end.txt\n" +
				"+++ b/end.txt\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"\\ No newline at end of file\n" +
				"+new\n" +
				"\\ No newline at end of file\n",
			want: []reviewFile{{
				Path: "end.txt", Status: "modified", Source: "unstaged", Added: 1, Removed: 1,
				Hunks: []reviewHunk{{Header: "@@ -1 +1 @@", Lines: []reviewLine{del(1, "old"), marker, add(1, "new"), marker}}},
			}},
		},
		{
			name: "binary files carry no hunks",
			raw: "diff --git a/img.png b/img.png\n" +
				"new file mode 100644\n" +
				"index 0000000..8888888\n" +
				"Binary files /dev/null and b/img.png differ\n",
			want: []reviewFile{{Path: "img.png", Status: "added", Source: "staged", Binary: true}},
		},
		{
			name: "adjacent files do not bleed into each other",
			raw: "diff --git a/one.txt b/one.txt\n" +
				"index 1..2 100644\n" +
				"--- a/one.txt\n" +
				"+++ b/one.txt\n" +
				"@@ -5,2 +5,2 @@\n" +
				" keep\n" +
				"-x\n" +
				"+y\n" +
				"diff --git a/two.txt b/two.txt\n" +
				"index 3..4 100644\n" +
				"--- a/two.txt\n" +
				"+++ b/two.txt\n" +
				"@@ -1,2 +1,3 @@\n" +
				" alpha\n" +
				"+beta\n" +
				" gamma\n",
			want: []reviewFile{
				{Path: "one.txt", Status: "modified", Source: "staged", Added: 1, Removed: 1,
					Hunks: []reviewHunk{{Header: "@@ -5,2 +5,2 @@", Lines: []reviewLine{ctx(5, 5, "keep"), del(6, "x"), add(6, "y")}}}},
				{Path: "two.txt", Status: "modified", Source: "staged", Added: 1,
					Hunks: []reviewHunk{{Header: "@@ -1,2 +1,3 @@", Lines: []reviewLine{ctx(1, 1, "alpha"), add(2, "beta"), ctx(2, 3, "gamma")}}}},
			},
		},
		{
			name: "quoted path with a tab is unquoted",
			raw: "diff --git \"a/odd\\tname.txt\" \"b/odd\\tname.txt\"\n" +
				"new file mode 100644\n" +
				"index 0000000..1111111\n" +
				"--- /dev/null\n" +
				"+++ \"b/odd\\tname.txt\"\n" +
				"@@ -0,0 +1 @@\n" +
				"+hi\n",
			want: []reviewFile{{Path: "odd\tname.txt", Status: "added", Source: "staged", Added: 1,
				Hunks: []reviewHunk{{Header: "@@ -0,0 +1 @@", Lines: []reviewLine{add(1, "hi")}}}}},
		},
		{
			name: "empty input yields no files",
			raw:  "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := "staged"
			if len(tc.want) > 0 {
				source = tc.want[0].Source
			}
			got, err := parseUnifiedDiff([]byte(tc.raw), source)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				gotJSON, _ := json.MarshalIndent(got, "", " ")
				wantJSON, _ := json.MarshalIndent(tc.want, "", " ")
				t.Fatalf("parse mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestParseUnifiedDiffTruncatedTail(t *testing.T) {
	raw := "diff --git a/one.txt b/one.txt\n--- a/one.txt\n+++ b/one.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+c\ndiff --git a/two.txt b/tw"
	files, err := parseUnifiedDiff([]byte(raw), "unstaged")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "one.txt" || len(files[0].Hunks) != 1 || len(files[0].Hunks[0].Lines) != 3 {
		t.Fatalf("a cut file header must be dropped, the complete file kept: %+v", files)
	}
}

func TestBoundedDiffOutput(t *testing.T) {
	const limit = 2 << 20
	// A multibyte line repeated past the limit: the cut must land on a
	// line boundary, never inside a code point.
	line := strings.Repeat("é日", 7) + "\n"
	var big bytes.Buffer
	for big.Len() <= limit+len(line)*3 {
		big.WriteString(line)
	}
	out, truncated, err := boundedRead(&big, limit)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("output over the limit must report truncation")
	}
	if len(out) > limit || len(out) == 0 {
		t.Fatalf("truncated output must stay within the limit: %d bytes", len(out))
	}
	if !utf8.Valid(out) {
		t.Fatal("truncated output must not end inside a code point")
	}
	if out[len(out)-1] != '\n' {
		t.Fatal("truncated output must end on a line boundary")
	}
	encoded, err := json.Marshal(string(out))
	if err != nil {
		t.Fatal(err)
	}
	var back string
	if err := json.Unmarshal(encoded, &back); err != nil || back != string(out) {
		t.Fatal("truncated output must round-trip through JSON unchanged")
	}

	small := "short\n"
	out, truncated, err = boundedRead(strings.NewReader(small), limit)
	if err != nil || truncated || string(out) != small {
		t.Fatalf("small output must pass through untouched: %q %v %v", out, truncated, err)
	}

	exact := strings.Repeat("x", limit)
	out, truncated, err = boundedRead(strings.NewReader(exact), limit)
	if err != nil || truncated || len(out) != limit {
		t.Fatalf("output exactly at the limit is not truncated: %d %v %v", len(out), truncated, err)
	}
}

// --- Git integration: real repositories under t.TempDir() ---

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// initTestRepo makes a repository with one commit holding a.txt and b.txt.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	writeRepoFile(t, dir, "a.txt", "one\ntwo\nthree\n")
	writeRepoFile(t, dir, "b.txt", "alpha\nbeta\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "init")
	return dir
}

// fakeSession registers a live-looking socket for this process's pid
// whose working directory resolves to dir.
func fakeSession(t *testing.T, dir string) (int, string) {
	t.Helper()
	// Short path: Unix socket names are capped at 104 bytes on macOS.
	runtime, err := os.MkdirTemp("", "acp-rt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtime) })
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	if err := os.MkdirAll(filepath.Join(runtime, "acp-multiplex"), 0700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	sock := filepath.Join(runtime, "acp-multiplex", strconv.Itoa(pid)+".sock")
	if err := os.WriteFile(sock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	previous := sessionCwd
	sessionCwd = func(int) string { return dir }
	t.Cleanup(func() { sessionCwd = previous })
	return pid, sock
}

func fileBySource(files []reviewFile, path, source string) *reviewFile {
	for i := range files {
		if files[i].Path == path && files[i].Source == source {
			return &files[i]
		}
	}
	return nil
}

func TestRepositoryReview(t *testing.T) {
	ctx := context.Background()
	t.Run("staged only", func(t *testing.T) {
		dir := initTestRepo(t)
		writeRepoFile(t, dir, "a.txt", "one\ntwo\nthree\nfour\n")
		gitIn(t, dir, "add", "a.txt")
		pid, _ := fakeSession(t, dir)
		res, err := repositoryReview(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Available || res.Scope != "repository" || res.Branch != "main" || res.Repository == "" {
			t.Fatalf("unexpected envelope: %+v", res)
		}
		if len(res.Files) != 1 || res.Files[0].Source != "staged" || res.Files[0].Added != 1 || res.Added != 1 {
			t.Fatalf("want one staged file with one addition: %+v", res.Files)
		}
		if got := res.Files[0].Hunks[0].Lines[len(res.Files[0].Hunks[0].Lines)-1]; got.Kind != "added" || got.New != 4 || got.Text != "four" {
			t.Fatalf("staged addition should be new line 4: %+v", got)
		}
	})
	t.Run("unstaged only", func(t *testing.T) {
		dir := initTestRepo(t)
		writeRepoFile(t, dir, "b.txt", "alpha\n")
		pid, _ := fakeSession(t, dir)
		res, err := repositoryReview(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Files) != 1 || res.Files[0].Source != "unstaged" || res.Files[0].Removed != 1 || res.Removed != 1 {
			t.Fatalf("want one unstaged file with one removal: %+v", res.Files)
		}
		if got := res.Files[0].Hunks[0].Lines[1]; got.Kind != "removed" || got.Old != 2 || got.Text != "beta" {
			t.Fatalf("removal should carry old line 2: %+v", got)
		}
	})
	t.Run("untracked files are additions from /dev/null", func(t *testing.T) {
		dir := initTestRepo(t)
		writeRepoFile(t, dir, "new dir/un tracked.txt", "u\n")
		writeRepoFile(t, dir, ".gitignore", "ignored.txt\n")
		writeRepoFile(t, dir, "ignored.txt", "x\n")
		gitIn(t, dir, "add", ".gitignore")
		gitIn(t, dir, "commit", "-qm", "ignore")
		pid, _ := fakeSession(t, dir)
		res, err := repositoryReview(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		f := fileBySource(res.Files, "new dir/un tracked.txt", "untracked")
		if f == nil || f.Status != "added" || f.Added != 1 || len(res.Files) != 1 {
			t.Fatalf("want exactly the untracked file as an addition: %+v", res.Files)
		}
		if f.Hunks[0].Lines[0].New != 1 || f.Hunks[0].Lines[0].Text != "u" {
			t.Fatalf("untracked content numbered from 1: %+v", f.Hunks[0].Lines)
		}
	})
	t.Run("partially staged file appears in both sections", func(t *testing.T) {
		dir := initTestRepo(t)
		writeRepoFile(t, dir, "a.txt", "one\ntwo\nthree\nfour\n")
		gitIn(t, dir, "add", "a.txt")
		writeRepoFile(t, dir, "a.txt", "one\ntwo\nthree\nfour\nfive\n")
		pid, _ := fakeSession(t, dir)
		res, err := repositoryReview(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		staged := fileBySource(res.Files, "a.txt", "staged")
		unstaged := fileBySource(res.Files, "a.txt", "unstaged")
		if staged == nil || unstaged == nil || len(res.Files) != 2 {
			t.Fatalf("want a staged and an unstaged patch: %+v", res.Files)
		}
		if staged.Added != 1 || staged.Hunks[0].Lines[len(staged.Hunks[0].Lines)-1].Text != "four" {
			t.Fatalf("staged patch is index vs HEAD: %+v", staged)
		}
		if unstaged.Added != 1 || unstaged.Hunks[0].Lines[len(unstaged.Hunks[0].Lines)-1].Text != "five" {
			t.Fatalf("unstaged patch must be relative to the index, not HEAD: %+v", unstaged)
		}
		if res.Added != 2 {
			t.Fatalf("totals sum both patches: %d", res.Added)
		}
	})
	t.Run("rename and deletion", func(t *testing.T) {
		dir := initTestRepo(t)
		gitIn(t, dir, "mv", "a.txt", "moved name.txt")
		gitIn(t, dir, "rm", "-q", "b.txt")
		pid, _ := fakeSession(t, dir)
		res, err := repositoryReview(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		moved := fileBySource(res.Files, "moved name.txt", "staged")
		gone := fileBySource(res.Files, "b.txt", "staged")
		if moved == nil || moved.Status != "renamed" || moved.OldPath != "a.txt" || len(moved.Hunks) != 0 {
			t.Fatalf("rename should keep old and new paths: %+v", res.Files)
		}
		if gone == nil || gone.Status != "deleted" || gone.Removed != 2 || gone.Hunks[0].Lines[1].Old != 2 {
			t.Fatalf("deletion should number old lines: %+v", res.Files)
		}
	})
	t.Run("binary file is labeled not rendered", func(t *testing.T) {
		dir := initTestRepo(t)
		writeRepoFile(t, dir, "blob.bin", "\x00\x01\x02\xff")
		gitIn(t, dir, "add", "blob.bin")
		pid, _ := fakeSession(t, dir)
		res, err := repositoryReview(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Files) != 1 || !res.Files[0].Binary || len(res.Files[0].Hunks) != 0 {
			t.Fatalf("binary staged file: %+v", res.Files)
		}
	})
	t.Run("empty repository without commits", func(t *testing.T) {
		dir := t.TempDir()
		gitIn(t, dir, "init", "-q", "-b", "main")
		writeRepoFile(t, dir, "first.txt", "hello\n")
		gitIn(t, dir, "add", "first.txt")
		writeRepoFile(t, dir, "second.txt", "world\n")
		pid, _ := fakeSession(t, dir)
		res, err := repositoryReview(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Available || res.Branch != "main" {
			t.Fatalf("empty repository must still review: %+v", res)
		}
		if f := fileBySource(res.Files, "first.txt", "staged"); f == nil || f.Status != "added" {
			t.Fatalf("staged file in an unborn branch is an addition: %+v", res.Files)
		}
		if f := fileBySource(res.Files, "second.txt", "untracked"); f == nil {
			t.Fatalf("untracked file listed: %+v", res.Files)
		}
	})
	t.Run("no changes", func(t *testing.T) {
		dir := initTestRepo(t)
		pid, _ := fakeSession(t, dir)
		res, err := repositoryReview(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Available || len(res.Files) != 0 || res.Reason == "" {
			t.Fatalf("clean tree: available with an explicit reason: %+v", res)
		}
		if string(mustJSON(t, res.Files)) != "[]" {
			t.Fatalf("files must encode as an empty array, got %s", mustJSON(t, res.Files))
		}
	})
	t.Run("not a git directory", func(t *testing.T) {
		dir := t.TempDir()
		pid, _ := fakeSession(t, dir)
		res, err := repositoryReview(ctx, pid)
		if err != nil {
			t.Fatal(err)
		}
		if res.Available || !strings.Contains(res.Reason, "Git repository") {
			t.Fatalf("non-git directory gives a precise reason: %+v", res)
		}
	})
	t.Run("unknown pid is an error", func(t *testing.T) {
		fakeSession(t, t.TempDir())
		if _, err := repositoryReview(ctx, 999999999); err == nil {
			t.Fatal("a pid without a live socket must be rejected")
		}
	})
	t.Run("review never touches index or objects", func(t *testing.T) {
		dir := initTestRepo(t)
		writeRepoFile(t, dir, "a.txt", "changed\n")
		writeRepoFile(t, dir, "u.txt", "new\n")
		before := repoFingerprint(t, dir)
		pid, _ := fakeSession(t, dir)
		if _, err := repositoryReview(ctx, pid); err != nil {
			t.Fatal(err)
		}
		if after := repoFingerprint(t, dir); after != before {
			t.Fatalf("repository review wrote to the repository:\n%s\n%s", before, after)
		}
	})
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// repoFingerprint summarises the index bytes and object names so a
// test can prove a review left them untouched.
func repoFingerprint(t *testing.T, dir string) string {
	t.Helper()
	index, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	filepath.WalkDir(filepath.Join(dir, ".git", "objects"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, strings.TrimPrefix(p, dir))
		}
		return nil
	})
	sort.Strings(names)
	return fmt.Sprintf("index=%x objects=%v", sha256.Sum256(index), names)
}

func TestTurnReview(t *testing.T) {
	ctx := context.Background()
	t.Run("snapshot holds the turn and stays immutable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir := initTestRepo(t)
		writeRepoFile(t, dir, "pre.txt", "before the turn\n")
		_, sock := fakeSession(t, dir)
		fingerprint := repoFingerprint(t, dir)

		baseline, err := beginTurnReview(ctx, "sess-1", sock)
		if err != nil {
			t.Fatal(err)
		}
		if got := loadTurnReview("sess-1"); !got.Pending || got.Available {
			t.Fatalf("a running capture reports pending: %+v", got)
		}
		writeRepoFile(t, dir, "a.txt", "one\ntwo\nthree\nfour\n")
		writeRepoFile(t, dir, "fresh.txt", "made in the turn\n")
		if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
			t.Fatal(err)
		}
		res, err := completeTurnReview(ctx, baseline)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Available || res.Scope != "turn" || res.Pending {
			t.Fatalf("completed turn: %+v", res)
		}
		for _, f := range res.Files {
			if f.Source != "turn" {
				t.Fatalf("turn files carry the turn source: %+v", f)
			}
		}
		if f := fileBySource(res.Files, "a.txt", "turn"); f == nil || f.Added != 1 {
			t.Fatalf("modified file in turn: %+v", res.Files)
		}
		if f := fileBySource(res.Files, "fresh.txt", "turn"); f == nil || f.Status != "added" {
			t.Fatalf("file created in turn: %+v", res.Files)
		}
		if f := fileBySource(res.Files, "b.txt", "turn"); f == nil || f.Status != "deleted" {
			t.Fatalf("file deleted in turn: %+v", res.Files)
		}
		if fileBySource(res.Files, "pre.txt", "turn") != nil {
			t.Fatalf("untracked file present before the turn is not part of it: %+v", res.Files)
		}
		if after := repoFingerprint(t, dir); after != fingerprint {
			t.Fatalf("turn capture wrote to the real repository:\n%s\n%s", fingerprint, after)
		}
		if entries, _ := os.ReadDir(filepath.Join(home, ".acp-mobile", "diff-captures")); len(entries) != 0 {
			t.Fatalf("capture directory must be removed after completion: %v", entries)
		}

		saved := loadTurnReview("sess-1")
		if saved.Pending || !saved.Available || !reflect.DeepEqual(saved.Files, res.Files) {
			t.Fatalf("saved snapshot must match the completed result: %+v", saved)
		}
		writeRepoFile(t, dir, "a.txt", "changed again after the turn\n")
		writeRepoFile(t, dir, "later.txt", "not in the turn\n")
		if again := loadTurnReview("sess-1"); !reflect.DeepEqual(again.Files, res.Files) {
			t.Fatal("a completed snapshot must not follow later repository edits")
		}
	})
	t.Run("abort leaves no snapshot and no capture directory", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir := initTestRepo(t)
		_, sock := fakeSession(t, dir)
		baseline, err := beginTurnReview(ctx, "sess-2", sock)
		if err != nil {
			t.Fatal(err)
		}
		writeRepoFile(t, dir, "a.txt", "changed\n")
		abortTurnReview(baseline)
		if got := loadTurnReview("sess-2"); got.Available || got.Pending {
			t.Fatalf("aborted turn must be unavailable: %+v", got)
		}
		if entries, _ := os.ReadDir(filepath.Join(home, ".acp-mobile", "diff-captures")); len(entries) != 0 {
			t.Fatalf("abort must remove the capture directory: %v", entries)
		}
	})
	t.Run("next completed turn replaces the snapshot", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		dir := initTestRepo(t)
		_, sock := fakeSession(t, dir)
		first, err := beginTurnReview(ctx, "sess-3", sock)
		if err != nil {
			t.Fatal(err)
		}
		writeRepoFile(t, dir, "first.txt", "1\n")
		if _, err := completeTurnReview(ctx, first); err != nil {
			t.Fatal(err)
		}
		second, err := beginTurnReview(ctx, "sess-3", sock)
		if err != nil {
			t.Fatal(err)
		}
		writeRepoFile(t, dir, "second.txt", "2\n")
		if _, err := completeTurnReview(ctx, second); err != nil {
			t.Fatal(err)
		}
		got := loadTurnReview("sess-3")
		if len(got.Files) != 1 || got.Files[0].Path != "second.txt" {
			t.Fatalf("latest turn wins: %+v", got.Files)
		}
	})
	t.Run("empty repository and no changes", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		dir := t.TempDir()
		gitIn(t, dir, "init", "-q", "-b", "main")
		_, sock := fakeSession(t, dir)
		baseline, err := beginTurnReview(ctx, "sess-4", sock)
		if err != nil {
			t.Fatal(err)
		}
		res, err := completeTurnReview(ctx, baseline)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Available || len(res.Files) != 0 || res.Reason == "" {
			t.Fatalf("no changes in an unborn repository: %+v", res)
		}
		baseline, err = beginTurnReview(ctx, "sess-4", sock)
		if err != nil {
			t.Fatal(err)
		}
		writeRepoFile(t, dir, "born.txt", "x\n")
		res, err = completeTurnReview(ctx, baseline)
		if err != nil || len(res.Files) != 1 || res.Files[0].Status != "added" {
			t.Fatalf("addition in an unborn repository: %+v %v", res.Files, err)
		}
	})
	t.Run("non-git directory cannot begin", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		_, sock := fakeSession(t, t.TempDir())
		if _, err := beginTurnReview(ctx, "sess-5", sock); err == nil {
			t.Fatal("begin must fail outside a repository")
		}
		if got := loadTurnReview("sess-5"); got.Available || got.Pending {
			t.Fatalf("failed begin leaves nothing behind: %+v", got)
		}
	})
	t.Run("startup sweep removes stale captures", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		stale := filepath.Join(home, ".acp-mobile", "diff-captures", "turn-stale")
		if err := os.MkdirAll(stale, 0700); err != nil {
			t.Fatal(err)
		}
		cleanupTurnCaptures()
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatal("stale capture directory must be swept on startup")
		}
	})
	t.Run("rejects an unsafe session id", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		_, sock := fakeSession(t, initTestRepo(t))
		if _, err := beginTurnReview(ctx, "../escape", sock); err == nil {
			t.Fatal("session id must be validated before it names a file")
		}
	})
}

func TestDiffReviewHandler(t *testing.T) {
	get := func(t *testing.T, query string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		handleDiffReview(rec, httptest.NewRequest(http.MethodGet, "/api/diff-review?"+query, nil))
		return rec
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) reviewResult {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("review responses must not be cached: %q", rec.Header().Get("Cache-Control"))
		}
		var res reviewResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	t.Setenv("HOME", t.TempDir())
	dir := initTestRepo(t)
	pid, sock := fakeSession(t, dir)

	rec := httptest.NewRecorder()
	handleDiffReview(rec, httptest.NewRequest(http.MethodPost, "/api/diff-review?scope=repository&pid=1", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST must be refused: %d", rec.Code)
	}
	for _, bad := range []string{"", "scope=nope", "scope=turn", "scope=turn&sessionId=../x", "scope=repository", "scope=repository&pid=abc"} {
		if rec := get(t, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q must be rejected with 400, got %d", bad, rec.Code)
		}
	}
	if rec := get(t, "scope=repository&pid=999999999"); rec.Code != http.StatusNotFound {
		t.Fatalf("a pid without a live socket is 404, got %d", rec.Code)
	}

	if res := decode(t, get(t, "scope=turn&sessionId=sess-h")); res.Available || res.Pending || res.Reason == "" || res.Files == nil {
		t.Fatalf("missing snapshot is an explicit unavailable state with an empty file list: %s", rec.Body.String())
	}

	baseline, err := beginTurnReview(context.Background(), "sess-h", sock)
	if err != nil {
		t.Fatal(err)
	}
	if res := decode(t, get(t, "scope=turn&sessionId=sess-h")); !res.Pending || res.Available {
		t.Fatalf("capture in progress is pending: %+v", res)
	}
	writeRepoFile(t, dir, "a.txt", "one\ntwo\nthree\nfour\n")
	if _, err := completeTurnReview(context.Background(), baseline); err != nil {
		t.Fatal(err)
	}
	res := decode(t, get(t, "scope=turn&sessionId=sess-h"))
	if !res.Available || res.Pending || len(res.Files) != 1 || res.Files[0].Source != "turn" || res.CapturedAt.IsZero() {
		t.Fatalf("completed turn response: %+v", res)
	}

	writeRepoFile(t, dir, "u.txt", "untracked\n")
	res = decode(t, get(t, "scope=repository&pid="+strconv.Itoa(pid)))
	if !res.Available || res.Scope != "repository" || fileBySource(res.Files, "u.txt", "untracked") == nil || fileBySource(res.Files, "a.txt", "unstaged") == nil {
		t.Fatalf("repository response: %+v", res)
	}
}

// TestBridgeCapturesTurnSnapshot drives a prompt through the real bridge
// against a fake agent that edits the repository before answering.
func TestBridgeCapturesTurnSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := initTestRepo(t)
	pid, _ := fakeSession(t, dir)
	sockPath := findSocket(strconv.Itoa(pid))
	os.Remove(sockPath) // replace the placeholder with a listening socket
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte(`{"jsonrpc":"2.0","method":"acp-multiplex/replay_complete"}` + "\n"))
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(line, &req) != nil || req.Method != "session/prompt" {
				continue
			}
			os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("written by the agent\n"), 0600)
			conn.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"stopReason":"end_turn"}}` + "\n"))
		}
	}()
	srv := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) { bridgeWebSocket(ws, sockPath) }))
	defer srv.Close()
	ws, err := websocket.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", "", "http://127.0.0.1/")
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var first string
	if err := websocket.Message.Receive(ws, &first); err != nil || !strings.Contains(first, "replay_complete") {
		t.Fatalf("expected replay_complete first: %q %v", first, err)
	}
	if err := websocket.Message.Send(ws, `{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"sessionId":"bridge-sess","prompt":[]}}`); err != nil {
		t.Fatal(err)
	}
	var reply string
	if err := websocket.Message.Receive(ws, &reply); err != nil || !strings.Contains(reply, "end_turn") {
		t.Fatalf("expected the prompt response: %q %v", reply, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := loadTurnReview("bridge-sess")
		if res.Available {
			if len(res.Files) != 1 || res.Files[0].Path != "agent.txt" || res.Files[0].Source != "turn" {
				t.Fatalf("snapshot should hold the agent's edit: %+v", res.Files)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot never completed: %+v", res)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
