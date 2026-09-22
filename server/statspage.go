package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

// The two routes the report is served from.
//
// They live under /images/'s sibling namespace rather than /public/, for the
// reason the concurrency limit measured: Mattermost handles /plugins/<id>/public/ itself and
// never forwards it, which produces a 404 that looks exactly like a broken
// link.
const (
	// statsPagePath is the page an administrator opens and reads.
	statsPagePath = "/report"
	// statsDataPath is the same figures as data, for a monitoring system or a
	// proof script. The page is for people; this is for machines.
	statsDataPath = "/report/data"
)

// serveStats answers both routes, or reports that it does not own the path.
func (p *MediaForgePlugin) serveStats(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path != statsPagePath && path != statsDataPath {
		return false
	}

	// Fail closed, and in this order. The figures say how much traffic this
	// instance carries and when it carries it, which is not something every
	// logged-in user gets to read — and the plugin's HTTP surface is reachable
	// by all of them, so the check has to be here.
	userID := r.Header.Get("Mattermost-User-Id")
	if userID == "" {
		http.Error(w, "please sign in", http.StatusUnauthorized)
		return true
	}
	if p.API == nil || !p.API.HasPermissionTo(userID, model.PermissionManageSystem) {
		http.Error(w, "this report is only available to system administrators",
			http.StatusForbidden)
		return true
	}

	report, err := p.statistics()
	if err != nil {
		p.logWarn("MediaForge: the statistics could not be read", "error", err.Error())
		http.Error(w, "the figures could not be read; please look at the server log",
			http.StatusInternalServerError)
		return true
	}

	// The figures change with every upload, so a cached copy would show an
	// administrator a number that was true a minute ago.
	w.Header().Set("Cache-Control", "no-store")

	if path == statsDataPath {
		w.Header().Set("Content-Type", "application/json")
		encoded, err := json.Marshal(report)
		if err != nil {
			http.Error(w, "the figures could not be prepared", http.StatusInternalServerError)
			return true
		}
		_, _ = w.Write(encoded)
		return true
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// No inline script and no external resource, so the page cannot be made to
	// load anything: it is rendered entirely from the figures.
	//
	// frame-ancestors is the half that is not about loading but about being
	// loaded. The page is plain HTML on the server's own origin, so without it
	// an attacker could lure a signed-in administrator into a page that frames
	// this one and read how much traffic the instance carries. X-Frame-Options
	// says the same thing for the older clients that ignore the policy.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	if err := renderStatsPage(w, report); err != nil {
		p.logWarn("MediaForge: the report page could not be rendered", "error", err.Error())
	}
	return true
}

// statsPageView is the report as the page shows it: every number already
// formatted, so the template holds no arithmetic.
type statsPageView struct {
	RetentionDays int
	Total         statsRowView
	// Kinds is the per-type rows in the order they are shown. It is a slice
	// rather than two named fields so the template holds no list of its own to
	// keep in step with the code.
	Kinds []statsRowView
	Days  []statsDayView
	Empty bool
}

// statsRowView is one row of figures, in words an administrator reads.
type statsRowView struct {
	Label      string
	Files      string
	Saved      string
	Sent       string
	Kept       string
	SavedShare string
	Failures   string
}

// statsDayView is one day of the trend.
type statsDayView struct {
	Day      string
	Files    string
	Saved    string
	Failures string
}

// renderStatsPage writes the page.
func renderStatsPage(w http.ResponseWriter, report statsReport) error {
	view := statsPageView{
		RetentionDays: report.RetentionDays,
		Total:         rowView("Everything together", report.Total),
		Kinds: []statsRowView{
			rowView("Images", report.Image),
			rowView("Videos", report.Video),
		},
		Empty: report.Total.Files == 0 && report.Total.Failures == 0,
	}
	for _, day := range report.Days {
		view.Days = append(view.Days, statsDayView{
			Day:      day.Day,
			Files:    formatCount(day.Total.Files),
			Saved:    formatBytesPlain(day.Total.BytesSaved),
			Failures: formatCount(day.Total.Failures),
		})
	}
	return statsTemplate.Execute(w, view)
}

// rowView formats one set of counters.
func rowView(label string, c statsCounters) statsRowView {
	return statsRowView{
		Label:      label,
		Files:      formatCount(c.Files),
		Saved:      formatBytesPlain(c.BytesSaved),
		Sent:       formatBytesPlain(c.BytesOriginal),
		Kept:       formatBytesPlain(c.BytesStored),
		SavedShare: formatShare(c.BytesOriginal, c.BytesStored),
		Failures:   formatCount(c.Failures),
	}
}

// formatCount groups a count with thin spaces so a large number stays
// readable.
func formatCount(n int64) string {
	text := strconv.FormatInt(n, 10)
	if len(text) <= 3 {
		return text
	}
	var out []byte
	for i, digit := range []byte(text) {
		if i > 0 && (len(text)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, digit)
	}
	return string(out)
}

// formatBytesPlain writes a byte count in decimal units.
//
// It deliberately does NOT reuse formatFileSize. The two answer different
// questions: the note tells a sender what one file saved and stops at TB,
// while this page adds up a whole server's storage and needs PB, and it spells
// small values "512 bytes" rather than "512 B" because a report is read as
// prose. Decimal units in both, for the same reason — every operating system
// an administrator checks the figure against counts a megabyte as a million
// bytes.
//
// Until the documentation pass there was a second reason, now gone: the note was written in
// German and this page was not.
func formatBytesPlain(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	if bytes < 1000 {
		return fmt.Sprintf("%d bytes", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	value := float64(bytes)
	for i, unit := range units {
		value /= 1000
		if value >= 1000 && i < len(units)-1 {
			continue
		}
		if unit == "KB" {
			return fmt.Sprintf("%.0f KB", value)
		}
		return fmt.Sprintf("%.1f %s", value, unit)
	}
	return fmt.Sprintf("%d bytes", bytes)
}

// formatShare is how much smaller the stored files are, in whole percent.
//
// It rounds down, so the page never claims a larger saving than the one that
// actually happened.
func formatShare(original, stored int64) string {
	if original <= 0 || stored >= original {
		return "0 %"
	}
	return fmt.Sprintf("%d %%", (original-stored)*100/original)
}

// statsTemplate is the whole page.
//
// html/template escapes every value, which matters because the figures are
// derived from data an ordinary user controls. Nothing here is loaded from
// outside, so the page works on an instance with no internet access — which is
// the kind of instance this plugin exists for.
var statsTemplate = template.Must(template.New("report").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>MediaForge — what it has saved</title>
<style>
 body { font: 15px/1.55 -apple-system, "Segoe UI", Roboto, Arial, sans-serif;
        margin: 0 auto; max-width: 54rem; padding: 2.5rem 1.25rem; color: #1b1d1f; }
 h1 { font-size: 1.5rem; margin: 0 0 .35rem; }
 h2 { font-size: 1.1rem; margin: 2.25rem 0 .5rem; }
 p.lead { color: #5d6368; margin: 0 0 2rem; }
 table { border-collapse: collapse; width: 100%; }
 th, td { text-align: left; padding: .5rem .65rem; border-bottom: 1px solid #e4e6e8; }
 th { font-weight: 600; color: #5d6368; font-size: .85rem; }
 td.n, th.n { text-align: right; font-variant-numeric: tabular-nums; }
 tr.sum td { font-weight: 600; background: #f7f8f9; }
 .big { font-size: 2rem; font-weight: 600; }
 .note { color: #5d6368; font-size: .9rem; margin-top: 2.5rem; }
 .empty { background: #f7f8f9; padding: 1.25rem; border-radius: .35rem; color: #5d6368; }
</style>
</head>
<body>
<h1>MediaForge — what it has saved</h1>
<p class="lead">Every file MediaForge made smaller while it was being uploaded,
over the last {{.RetentionDays}} days.</p>

{{if .Empty}}
<p class="empty">MediaForge has not made any file smaller yet. That is what a
freshly installed plugin looks like, and it is also what you see if every
upload so far was already small enough to leave alone.</p>
{{else}}
<p class="big">{{.Total.Saved}} of storage space saved</p>
<p class="lead">{{.Total.Files}} file(s) were made smaller: {{.Total.Sent}} were
sent, {{.Total.Kept}} are kept — {{.Total.SavedShare}} less.</p>
{{end}}

<h2>By kind of file</h2>
<table>
<tr><th>Kind</th><th class="n">Files made smaller</th><th class="n">Storage space saved</th>
    <th class="n">Share</th><th class="n">Could not be converted</th></tr>
{{range $row := .Kinds}}
<tr><td>{{$row.Label}}</td><td class="n">{{$row.Files}}</td><td class="n">{{$row.Saved}}</td>
    <td class="n">{{$row.SavedShare}}</td><td class="n">{{$row.Failures}}</td></tr>
{{end}}
<tr class="sum"><td>{{.Total.Label}}</td><td class="n">{{.Total.Files}}</td>
    <td class="n">{{.Total.Saved}}</td><td class="n">{{.Total.SavedShare}}</td>
    <td class="n">{{.Total.Failures}}</td></tr>
</table>

<h2>Day by day</h2>
{{if .Days}}
<table>
<tr><th>Day</th><th class="n">Files made smaller</th><th class="n">Storage space saved</th>
    <th class="n">Could not be converted</th></tr>
{{range .Days}}
<tr><td>{{.Day}}</td><td class="n">{{.Files}}</td><td class="n">{{.Saved}}</td>
    <td class="n">{{.Failures}}</td></tr>
{{end}}
</table>
{{else}}
<p class="empty">No day in the last {{.RetentionDays}} days has any figures yet.</p>
{{end}}

<p class="note">A day on which nothing was made smaller has no row at all, so an
empty list means no uploads needed shrinking rather than something being
broken. Files that were already small, pictures MediaForge cannot read, and
videos that were already saved efficiently are stored exactly as they were
sent; they save nothing and are not counted here. "Could not be converted"
counts uploads that were refused because the video was damaged or took too
long — the sender was told and can try again. Figures older than
{{.RetentionDays}} days are removed automatically.</p>
</body>
</html>
`))
