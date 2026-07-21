package web

import (
	"reflect"
	"strings"
	"testing"
)

// The phrase is the whole gate. It has to be forgiving of how text arrives —
// a copy-paste that picked up a line break is not a failure to understand the
// warning — while staying strict about the words and their capitalisation,
// which is the part that makes someone read it.
func TestPhraseMatches(t *testing.T) {
	good := []string{
		ConsolePhrase,
		"  " + ConsolePhrase + "  ",
		strings.ReplaceAll(ConsolePhrase, " ", "  "),
		strings.Replace(ConsolePhrase, "may", "may\n", 1),
		"\t" + ConsolePhrase + "\r\n",
	}
	for _, s := range good {
		if !phraseMatches(s) {
			t.Errorf("phraseMatches(%q) = false, want true", truncate(s))
		}
	}

	bad := []string{
		"",
		"yes",
		"I understand",
		strings.ToUpper(ConsolePhrase),
		strings.ToLower(ConsolePhrase),
		strings.TrimSuffix(ConsolePhrase, " backup"),
		ConsolePhrase + " really",
		strings.Replace(ConsolePhrase, "irrevocably", "irrevocably ", 1) + "x",
		strings.Replace(ConsolePhrase, "database", "databases", 1),
	}
	for _, s := range bad {
		if phraseMatches(s) {
			t.Errorf("phraseMatches(%q) = true, want false", truncate(s))
		}
	}
}

func truncate(s string) string {
	if len(s) > 50 {
		return s[:50] + "…"
	}
	return s
}

// splitStatements is not a SQL parser, but the cases it does have to survive
// are exactly the ones a strings.Split(";") gets wrong — and getting them wrong
// in a console that writes to a live database means running half a statement.
func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"single", "SELECT 1", []string{"SELECT 1"}},
		{"trailing semicolon", "SELECT 1;", []string{"SELECT 1"}},
		{"two statements", "SELECT 1; SELECT 2;", []string{"SELECT 1", " SELECT 2"}},
		{
			"semicolon inside a string literal",
			"INSERT INTO t VALUES ('a;b');",
			[]string{"INSERT INTO t VALUES ('a;b')"},
		},
		{
			"escaped quote inside a string",
			"INSERT INTO t VALUES ('it''s; fine');",
			[]string{"INSERT INTO t VALUES ('it''s; fine')"},
		},
		{
			"semicolon inside a double-quoted identifier",
			`SELECT "a;b" FROM t;`,
			[]string{`SELECT "a;b" FROM t`},
		},
		{
			"semicolon inside a line comment",
			"SELECT 1 -- ; not a boundary\n;SELECT 2",
			[]string{"SELECT 1 -- ; not a boundary\n", "SELECT 2"},
		},
		{
			"semicolon inside a block comment",
			"SELECT 1 /* ; still not */ ; SELECT 2",
			[]string{"SELECT 1 /* ; still not */ ", " SELECT 2"},
		},
		{"empty input", "", nil},
		{"only whitespace", "   \n  ", nil},
		{"only a semicolon", ";", []string{""}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitStatements(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("splitStatements(%q)\n got %#v\nwant %#v", c.in, got, c.want)
			}
		})
	}
}

// An unterminated string must not swallow the rest of the script silently —
// it should come back as one statement and let SQLite report the syntax error,
// rather than being split at a semicolon that is inside the literal.
func TestSplitStatementsUnterminatedString(t *testing.T) {
	got := splitStatements("SELECT 'oops; SELECT 2;")
	if len(got) != 1 {
		t.Fatalf("expected the unterminated literal to stay one statement, got %#v", got)
	}
}

func TestRenderCell(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "∅"},
		{int64(42), "42"},
		{"hello", "hello"},
		{"NULL", "NULL"}, // the string, which must not look like a real NULL
		{[]byte{0xde, 0xad}, "x'dead'"},
	}
	for _, c := range cases {
		if got := renderCell(c.in); got != c.want {
			t.Errorf("renderCell(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
	// A large blob is summarised rather than dumped as hex across the page.
	big := renderCell(make([]byte, 200))
	if !strings.Contains(big, "200 bytes") {
		t.Errorf("large blob rendered as %q, want a byte-count summary", big)
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("DROP TABLE t;\nSELECT 1;"); got != "DROP TABLE t;" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine(strings.Repeat("x", 300)); len(got) > 200 {
		t.Errorf("firstLine did not truncate: %d chars", len(got))
	}
}
