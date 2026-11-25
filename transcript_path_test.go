package main

import "testing"

// Windows paths are case-insensitive, and Emacs there reports a lower-case
// drive letter (c:\Users\x) where os.UserHomeDir says C:\Users\x.
func TestPathUnderDirFoldsCaseOnlyWhenAsked(t *testing.T) {
	cases := []struct {
		path, dir, sep string
		fold, want     bool
	}{
		{`c:\Users\x\proj\.agent-shell\transcripts\a.md`, `C:\Users\x`, `\`, true, true},
		{`c:\Users\x\proj\.agent-shell\transcripts\a.md`, `C:\Users\x`, `\`, false, false},
		{`C:\Users\xy\a.md`, `C:\Users\x`, `\`, true, false},
		{`/Users/x/proj/a.md`, `/Users/x`, `/`, false, true},
		{`/users/x/proj/a.md`, `/Users/x`, `/`, false, false},
		{`/Users/x`, `/Users/x`, `/`, false, false},
	}
	for _, c := range cases {
		if got := pathUnderDir(c.path, c.dir, c.sep, c.fold); got != c.want {
			t.Errorf("pathUnderDir(%q, %q, fold=%v) = %v, want %v", c.path, c.dir, c.fold, got, c.want)
		}
	}
}
