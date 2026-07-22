package web

import (
	"testing"
	"time"

	"muxxerr/internal/supervisor"
)

func at(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// formatLine is what turns a wall of escaped JSON into a readable page, so the
// cases that matter are the ones where the input is NOT the tidy JSON the apps
// usually emit — a panic, a truncated line, a plain-text warning from the Go
// runtime. Those must survive intact, because they are the output somebody is
// most likely to be reading the page to find.
func TestFormatLine(t *testing.T) {
	cases := []struct {
		name           string
		in             supervisor.LogLine
		wantText       string
		wantLevel      string
		wantFields     string
		wantIsErr      bool
		wantTimePrefix string
	}{
		{
			name: "structured json is unpacked",
			in: supervisor.LogLine{
				At: at("2026-07-21T10:00:00Z"), Stream: "stdout",
				Text: `{"time":"2026-07-21T12:34:56.789Z","level":"INFO","msg":"readerr backend listening","addr":":58912","db":"/data/readerr.db"}`,
			},
			wantText:   "readerr backend listening",
			wantLevel:  "INFO",
			wantFields: `addr=:58912 db=/data/readerr.db`,
		},
		{
			name: "ERROR level marks the row even on stdout",
			in: supervisor.LogLine{
				At: at("2026-07-21T10:00:00Z"), Stream: "stdout",
				Text: `{"level":"ERROR","msg":"open database","error":"disk full"}`,
			},
			wantText: "open database", wantLevel: "ERROR",
			wantFields: "error=disk full", wantIsErr: true,
		},
		{
			name: "a panic is not JSON and must survive verbatim",
			in: supervisor.LogLine{
				At: at("2026-07-21T10:00:00Z"), Stream: "stderr",
				Text: "panic: runtime error: invalid memory address or nil pointer dereference",
			},
			wantText:  "panic: runtime error: invalid memory address or nil pointer dereference",
			wantIsErr: true,
		},
		{
			name: "a truncated JSON line is shown raw rather than swallowed",
			in: supervisor.LogLine{
				At: at("2026-07-21T10:00:00Z"), Stream: "stdout",
				Text: `{"level":"INFO","msg":"half a li`,
			},
			wantText: `{"level":"INFO","msg":"half a li`,
		},
		{
			name: "JSON without a msg is not slog output; leave it alone",
			in: supervisor.LogLine{
				At: at("2026-07-21T10:00:00Z"), Stream: "stdout",
				Text: `{"some":"other json"}`,
			},
			wantText: `{"some":"other json"}`,
		},
		{
			name: "stderr is flagged even when the payload looks routine",
			in: supervisor.LogLine{
				At: at("2026-07-21T10:00:00Z"), Stream: "stderr",
				Text: "go: downloading something",
			},
			wantText: "go: downloading something", wantIsErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatLine(c.in)
			if got.Text != c.wantText {
				t.Errorf("Text = %q, want %q", got.Text, c.wantText)
			}
			if got.Level != c.wantLevel {
				t.Errorf("Level = %q, want %q", got.Level, c.wantLevel)
			}
			if got.Fields != c.wantFields {
				t.Errorf("Fields = %q, want %q", got.Fields, c.wantFields)
			}
			if got.IsErr != c.wantIsErr {
				t.Errorf("IsErr = %v, want %v", got.IsErr, c.wantIsErr)
			}
			if got.At == "" {
				t.Error("At is empty")
			}
		})
	}
}

// Field ordering has to be stable or the same log line reshuffles on every
// reload, which makes a page you are staring at impossible to read.
func TestFormatLineFieldOrderIsStable(t *testing.T) {
	line := supervisor.LogLine{
		At: at("2026-07-21T10:00:00Z"), Stream: "stdout",
		Text: `{"level":"INFO","msg":"m","zulu":1,"alpha":2,"mike":3,"bravo":4}`,
	}
	want := "alpha=2 bravo=4 mike=3 zulu=1"
	for i := 0; i < 20; i++ {
		if got := formatLine(line).Fields; got != want {
			t.Fatalf("iteration %d: Fields = %q, want %q", i, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "empty"},
		{-1, "empty"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		// Three significant figures throughout: below 100 the decimal earns its
		// place, at or above it the extra digit is noise.
		{99 * 1024, "99.0 KB"},
		{605184, "591 KB"},
		{1024 * 1024, "1.0 MB"},
		{4_613_734, "4.4 MB"},
		{150 * 1024 * 1024, "150 MB"}, // >=100 drops the decimal
		{3 * 1024 * 1024 * 1024, "3.0 GB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLevelClass(t *testing.T) {
	cases := map[string]string{
		"ERROR": "err", "error": "err", "FATAL": "err",
		"WARN": "warn", "warning": "warn",
		"INFO": "", "DEBUG": "", "": "",
	}
	for in, want := range cases {
		if got := levelClass(in); got != want {
			t.Errorf("levelClass(%q) = %q, want %q", in, got, want)
		}
	}
}
