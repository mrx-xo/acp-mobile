package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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
