package main

import (
	"strings"
	"testing"
)

func TestRenderChangelogBasic(t *testing.T) {
	// The exact content the apps ship.
	in := "# 0.1.0 July 22nd 2026\n\n- initial release\n"
	out := renderChangelogHTML(in)
	for _, want := range []string{
		"<h1>0.1.0 July 22nd 2026</h1>",
		"<ul>",
		"<li>initial release</li>",
		"</ul>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered changelog missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestRenderChangelogInline(t *testing.T) {
	in := "## Changes\n\nAdded **bold**, *italic*, `code`, and a [link](https://example.com).\n"
	out := renderChangelogHTML(in)
	for _, want := range []string{
		"<h2>Changes</h2>",
		"<strong>bold</strong>",
		"<em>italic</em>",
		"<code>code</code>",
		`<a href="https://example.com" rel="noopener noreferrer">link</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\ngot:\n%s", want, out)
		}
	}
}

// The one that actually matters: content from an app repo is injected into the
// gateway's own page, so nothing in a CHANGELOG may become live markup.
func TestRenderChangelogIsXSSSafe(t *testing.T) {
	in := "# <script>alert(1)</script>\n\n" +
		"- <img src=x onerror=alert(1)>\n" +
		"- [click](javascript:alert(1))\n" +
		"- [ok](https://safe.example/pr/1)\n"
	out := renderChangelogHTML(in)

	if strings.Contains(out, "<script>") || strings.Contains(out, "<img") {
		t.Errorf("raw HTML survived escaping:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "javascript:") {
		t.Errorf("javascript: URL was not stripped:\n%s", out)
	}
	// The escaped forms should be present (rendered as visible text).
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("expected escaped <script> text\ngot:\n%s", out)
	}
	// The unsafe link keeps its visible text but loses the href.
	if !strings.Contains(out, "click") || strings.Contains(out, `href="javascript`) {
		t.Errorf("javascript link not neutralised:\n%s", out)
	}
	// A safe link still renders.
	if !strings.Contains(out, `href="https://safe.example/pr/1"`) {
		t.Errorf("safe link missing:\n%s", out)
	}
}

func TestRenderChangelogCodeFence(t *testing.T) {
	in := "```\n<not a tag>\n```\n"
	out := renderChangelogHTML(in)
	if !strings.Contains(out, "<pre><code>") || !strings.Contains(out, "&lt;not a tag&gt;") {
		t.Errorf("code fence not rendered/escaped:\n%s", out)
	}
}

func TestSafeURL(t *testing.T) {
	ok := []string{"https://a.com", "http://a.com", "mailto:x@a.com", "/rel", "#frag", "./x", "../y", "page.html", "a.com/x"}
	bad := []string{"javascript:alert(1)", "data:text/html,x", "vbscript:x", "", "  "}
	for _, u := range ok {
		if !safeURL(u) {
			t.Errorf("safeURL(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if safeURL(u) {
			t.Errorf("safeURL(%q) = true, want false", u)
		}
	}
}
