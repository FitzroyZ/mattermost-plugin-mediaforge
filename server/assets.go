package main

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// noteImages holds the System Console preview images.
//
// They are embedded in the binary rather than read from disk: a plugin bundle
// is unpacked to a path the plugin cannot rely on, and an image that fails to
// load would leave an administrator staring at a broken icon next to the very
// setting it is supposed to explain.
//
//go:embed public/*.png
var noteImages embed.FS

// voiceSettingsPath is the route the webapp asks before it draws the record
// button.
const voiceSettingsPath = "/voice/settings"

// voiceSettingsResponse is what that route answers.
//
// It carries the file name prefix as well as the switch, so the two sides of
// the contract cannot drift apart: the webapp names its upload from what the
// server told it, rather than from a string copied into the JavaScript. The
// duration limit travels the same way and for the same reason — two copies of
// one rule drift apart, and the symptom would be a browser that stops at ten
// minutes while the server refuses at five.
type voiceSettingsResponse struct {
	// Enabled reports whether a recording would actually be converted — in
	// THIS channel, when one was asked about.
	Enabled bool `json:"enabled"`
	// NamePrefix is the prefix the webapp must put on its upload.
	NamePrefix string `json:"name_prefix"`
	// MaxSeconds is the longest recording the server will accept, so the
	// browser can stop in time rather than let somebody speak for eleven
	// minutes and then have the upload refused.
	MaxSeconds int `json:"max_seconds"`
}

// serveVoiceSettings answers the voice-settings route, or reports that it does
// not own the path.
//
// A session is required and nothing more. The answer says whether this server
// converts recordings — which every logged-in user needs before the button can
// be drawn, and which says nothing an administrator would want to keep from
// them. It is deliberately NOT open to anonymous callers: the plugin's HTTP
// surface should not answer questions about the server's configuration to
// someone who is not signed in at all.
//
// The channel arrives as a query parameter because the webapp knows it: the
// post editor component receives the whole PostDraft, which carries channelId.
// The upload callback from registerFileUploadMethod does NOT — it is called
// with nothing but itself (file_upload.tsx line 659) — which is why the button
// is driven from the component's draft and why this route has to be able to
// answer per channel.
//
// Asked WITHOUT a channel while a channel list is configured, the answer is
// "not available". The route cannot know where the caller is, and claiming
// availability it cannot promise would draw a button in a channel the server
// then declines to convert for — which is worse than no button, because it
// offers something and then takes it away.
func (p *MediaForgePlugin) serveVoiceSettings(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSuffix(r.URL.Path, "/") != voiceSettingsPath {
		return false
	}

	if r.Header.Get("Mattermost-User-Id") == "" {
		http.Error(w, "please sign in", http.StatusUnauthorized)
		return true
	}

	cfg := p.getConfiguration()

	// The answer reflects BOTH halves — the administrator's switch and whether
	// ffmpeg is installed. Offering a button on a server that cannot convert
	// would produce recordings that play on a desktop and stay silent on a
	// phone, which is the failure SPEC §4.3 exists to rule out.
	offered := cfg.voiceEnabled()

	enabled := offered
	if enabled {
		enabled = p.voiceAllowedInChannel(cfg, &model.FileInfo{
			ChannelId: r.URL.Query().Get("channel_id"),
		})
	}

	// The prefix is sent only by a server that offers the feature at all, and
	// the webapp uses its presence to decide whether to register anything.
	//
	// The two flags answer different questions and both are needed. `enabled`
	// is per channel and can be false in a channel while the feature is
	// perfectly available elsewhere — so the webapp cannot use it to decide
	// whether to register a button at all, or a channel list would remove the
	// button everywhere. The prefix says "this server does voice messages",
	// which is exactly the server-wide question.
	prefix := ""
	if offered {
		prefix = voiceRecordingPrefix
	}

	body, err := json.Marshal(voiceSettingsResponse{
		Enabled:    enabled,
		NamePrefix: prefix,
		MaxSeconds: cfg.MaxVoiceSeconds,
	})

	if err != nil {
		http.Error(w, "the settings could not be prepared", http.StatusInternalServerError)
		return true
	}

	w.Header().Set("Content-Type", "application/json")
	// An administrator can change this at any moment and the button has to
	// follow on the next page load, so a cached answer would show a stale one.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
	return true
}

// ServeHTTP serves the plugin's own static files, its statistics report and
// the one question the webapp asks before it draws the record button.
//
// The surface is deliberately tiny: the preview images under /images/, the
// report under /report, and /voice/settings. Only GET; the report requires
// system administrator permission (see serveStats) and the voice settings
// require a session (see serveVoiceSettings).
func (p *MediaForgePlugin) ServeHTTP(_ *plugin.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// The report is checked first because it is the only route with a
	// permission check, and an ordering that could fall through to the static
	// handler would be a way around it.
	if p.serveStats(w, r) {
		return
	}

	if p.serveVoiceSettings(w, r) {
		return
	}

	// Also before the static handler, and for the same reason: the health
	// report names absolute paths and free disk space, so it carries its own
	// system-administrator check (see serveHealth).
	if p.serveHealth(w, r) {
		return
	}

	// The route is deliberately NOT called "public": Mattermost handles
	// /plugins/<id>/public/ itself and never forwards it to the plugin, so a
	// file served from there is a 404 that looks like an embedding bug.
	// Measured on the test instance before choosing this name.
	const route = "/images/"
	if !strings.HasPrefix(r.URL.Path, route) || strings.Contains(r.URL.Path, "..") ||
		!strings.HasSuffix(r.URL.Path, ".png") {
		http.NotFound(w, r)
		return
	}
	name := "public/" + strings.TrimPrefix(r.URL.Path, route)
	data, err := noteImages.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	// The images ship with the plugin version, so they may be cached hard.
	w.Header().Set("Cache-Control", "public, max-age=604800")
	// nosniff is the audit's answer to gosec's G705 on the line below.
	//
	// The finding itself is a false positive and is rejected in PLAN §the security audit:
	// `data` comes from an embed.FS restricted to `public/*.png`, so the bytes
	// are compiled into the binary and no caller can influence them — the route
	// can only ever return one of five known images.
	//
	// The header is set anyway because it costs one line and closes the
	// general shape of the finding rather than today's instance of it: if a
	// later change ever served a file whose bytes an uploader chose, a browser
	// that sniffed the content type would be the difference between a download
	// and script execution on the server's own origin. Declaring the type
	// authoritative here means that change would have to defeat a stated
	// guarantee rather than an accident.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}
