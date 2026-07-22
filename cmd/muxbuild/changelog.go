// Rendering an app's CHANGELOG.md into HTML at build time.
//
// The result is injected verbatim into the muxxerr dashboard — the gateway's
// own trusted page — so the one non-negotiable requirement is that a hostile or
// careless CHANGELOG cannot smuggle markup into it. Every scrap of source text
// is HTML-escaped first; this converter then emits only a fixed, safe set of
// tags of its own, and link targets are scheme-checked. There is no raw-HTML
// passthrough.
//
// It is deliberately small — headings, lists, paragraphs, rules, fenced and
// inline code, and inline bold/italic/links. "No fancy parsing": a changelog
// that leans on nested lists or tables will render flat, and that is the app
// author's call to keep it simple, per the feature's contract.
package main

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

var (
	reHeading  = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	reULItem   = regexp.MustCompile(`^\s*[-*+]\s+(.*)$`)
	reOLItem   = regexp.MustCompile(`^\s*\d+[.)]\s+(.*)$`)
	reCodeSpan = regexp.MustCompile("`([^`]+)`")
	reLink     = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	reBold     = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic   = regexp.MustCompile(`\*([^*]+)\*`)
)

// isRule reports a thematic break: three or more of the same -, * or _, with
// any spacing. Go's RE2 has no backreferences, so this cannot be one regex.
func isRule(line string) bool {
	s := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(line))
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] != c {
			return false
		}
	}
	return true
}

// renderChangelogHTML converts CHANGELOG markdown to a safe HTML fragment.
func renderChangelogHTML(src string) string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	lines := strings.Split(src, "\n")

	var b strings.Builder
	var para []string
	inUL, inOL, inCode := false, false, false

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		b.WriteString("<p>")
		for i, l := range para {
			if i > 0 {
				b.WriteString("<br>")
			}
			b.WriteString(inlineMarkdown(html.EscapeString(l)))
		}
		b.WriteString("</p>\n")
		para = para[:0]
	}
	closeLists := func() {
		if inUL {
			b.WriteString("</ul>\n")
			inUL = false
		}
		if inOL {
			b.WriteString("</ol>\n")
			inOL = false
		}
	}

	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inCode {
				b.WriteString("</code></pre>\n")
				inCode = false
			} else {
				flushPara()
				closeLists()
				b.WriteString("<pre><code>")
				inCode = true
			}
			continue
		}
		if inCode {
			b.WriteString(html.EscapeString(line))
			b.WriteString("\n")
			continue
		}
		if strings.TrimSpace(line) == "" {
			flushPara()
			closeLists()
			continue
		}
		if isRule(line) {
			flushPara()
			closeLists()
			b.WriteString("<hr>\n")
			continue
		}
		if m := reHeading.FindStringSubmatch(line); m != nil {
			flushPara()
			closeLists()
			level := len(m[1])
			b.WriteString(fmt.Sprintf("<h%d>%s</h%d>\n", level, inlineMarkdown(html.EscapeString(m[2])), level))
			continue
		}
		if m := reULItem.FindStringSubmatch(line); m != nil {
			flushPara()
			if inOL {
				b.WriteString("</ol>\n")
				inOL = false
			}
			if !inUL {
				b.WriteString("<ul>\n")
				inUL = true
			}
			b.WriteString("<li>" + inlineMarkdown(html.EscapeString(m[1])) + "</li>\n")
			continue
		}
		if m := reOLItem.FindStringSubmatch(line); m != nil {
			flushPara()
			if inUL {
				b.WriteString("</ul>\n")
				inUL = false
			}
			if !inOL {
				b.WriteString("<ol>\n")
				inOL = true
			}
			b.WriteString("<li>" + inlineMarkdown(html.EscapeString(m[1])) + "</li>\n")
			continue
		}
		para = append(para, line)
	}
	if inCode {
		b.WriteString("</code></pre>\n")
	}
	flushPara()
	closeLists()
	return b.String()
}

// inlineMarkdown applies inline formatting to already-HTML-escaped text. Because
// the input is escaped, `<`, `>`, `&`, `"` and `'` are already inert; this only
// ever adds tags of its own choosing.
func inlineMarkdown(escaped string) string {
	// Protect code spans first, so ** or [ ] inside `code` is left literal.
	var codes []string
	s := reCodeSpan.ReplaceAllStringFunc(escaped, func(m string) string {
		inner := m[1 : len(m)-1]
		codes = append(codes, "<code>"+inner+"</code>")
		return "\x00" + strconv.Itoa(len(codes)-1) + "\x00"
	})

	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		g := reLink.FindStringSubmatch(m)
		text, url := g[1], g[2]
		if !safeURL(url) {
			return text // unsafe scheme: drop the link, keep the visible text
		}
		// url came out of already-escaped text, so &, <, >, " and ' are already
		// entity-encoded — it is safe to place in the attribute as-is.
		return `<a href="` + url + `" rel="noopener noreferrer">` + text + `</a>`
	})

	s = reBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = reItalic.ReplaceAllString(s, "<em>$1</em>")

	for i, c := range codes {
		s = strings.Replace(s, "\x00"+strconv.Itoa(i)+"\x00", c, 1)
	}
	return s
}

// safeURL allows only link targets that cannot execute script: http(s), mailto,
// anchors, and relative paths. Anything with a scheme we do not recognise
// (javascript:, data:, vbscript:, …) is rejected.
func safeURL(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" {
		return false
	}
	if strings.HasPrefix(u, "/") || strings.HasPrefix(u, "#") ||
		strings.HasPrefix(u, "./") || strings.HasPrefix(u, "../") {
		return true
	}
	lc := strings.ToLower(u)
	for _, ok := range []string{"http://", "https://", "mailto:"} {
		if strings.HasPrefix(lc, ok) {
			return true
		}
	}
	// No scheme at all (e.g. "example.com/x", "page.html") is a relative
	// reference and is safe; a colon means an unrecognised scheme, so reject.
	return !strings.Contains(lc, ":")
}
