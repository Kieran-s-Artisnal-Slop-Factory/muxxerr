package web

import (
	"html/template"
	"strings"
	"testing"
)

// The dashboard badge is the whole point of the app-build feature, so pin that
// it renders when there is build info and — just as important — leaves the card
// untouched when there is not.
func TestChooserRendersBuildBadge(t *testing.T) {
	tmpl, err := template.New("chooser").Funcs(funcs).
		ParseFS(templateFS, "templates/layout.html", "templates/chooser.html")
	if err != nil {
		t.Fatalf("parse chooser: %v", err)
	}

	v := chooserView{
		Page:     PageData{Title: "Your apps", SiteName: "Muxxerr"},
		Username: "alice",
		Installed: []installedApp{{
			Name: "readerr", Title: "Readerr", Description: "Reading list",
			URL: "/alice/readerr/", Running: true, Added: "1 Jan 2026", Size: "1 KB",
			Build: "1.4.0 · 3035d7f2a", BuildTitle: "commit 3035d7f2a6b1",
			Changelog: template.HTML("<h1>1.4.0</h1>\n<ul>\n<li>a change</li>\n</ul>\n"),
		}},
		Available: []availableApp{{
			Name: "workoutt", Title: "Workoutt", Description: "Workouts",
			Build: "2a516fce8", BuildTitle: "commit 2a516fce80a4",
		}},
	}

	render := func() string {
		var buf strings.Builder
		if err := tmpl.ExecuteTemplate(&buf, "layout", v); err != nil {
			t.Fatalf("render: %v", err)
		}
		return buf.String()
	}

	out := render()
	for _, want := range []string{
		`class="build-badge"`,
		"1.4.0 · 3035d7f2a", // installed card badge
		"2a516fce8",         // available item badge
		`title="commit 3035d7f2a6b1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered dashboard is missing %q", want)
		}
	}

	// No build info → no badge, card unchanged.
	v.Installed[0].Build = ""
	v.Available[0].Build = ""
	if strings.Contains(render(), "build-badge") {
		t.Error("badge rendered even though Build was empty")
	}
}

func TestChooserRendersReleaseNotesDialog(t *testing.T) {
	tmpl, err := template.New("chooser").Funcs(funcs).
		ParseFS(templateFS, "templates/layout.html", "templates/chooser.html")
	if err != nil {
		t.Fatalf("parse chooser: %v", err)
	}
	v := chooserView{
		Page:     PageData{Title: "Your apps", SiteName: "Muxxerr"},
		Username: "alice",
		Installed: []installedApp{{
			Name: "readerr", Title: "Readerr", URL: "/alice/readerr/",
			Changelog: template.HTML("<h1>1.4.0</h1>\n<ul>\n<li>a change</li>\n</ul>\n"),
		}},
	}
	render := func() string {
		var buf strings.Builder
		if err := tmpl.ExecuteTemplate(&buf, "layout", v); err != nil {
			t.Fatalf("render: %v", err)
		}
		return buf.String()
	}

	out := render()
	for _, want := range []string{
		`data-open-dialog="notes-readerr"`,                    // the trigger button
		`<dialog class="changelog-dialog" id="notes-readerr"`, // the modal
		`<li>a change</li>`,                                   // the rendered changelog HTML, unescaped
		`method="dialog"`,                                     // the no-JS close button
	} {
		if !strings.Contains(out, want) {
			t.Errorf("release-notes modal missing %q", want)
		}
	}

	// No changelog → no trigger button and no dialog element. (Check the actual
	// markup, not the bare class name, which also appears in the always-present
	// wiring script.)
	v.Installed[0].Changelog = ""
	out = render()
	if strings.Contains(out, `data-open-dialog="notes-`) ||
		strings.Contains(out, `<dialog class="changelog-dialog"`) {
		t.Error("release-notes UI rendered even though there was no changelog")
	}
}
