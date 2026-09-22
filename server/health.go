package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// The health report exists because a missing dependency used to be INVISIBLE.
//
// detectMediaTools (ffmpeg.go) has always noticed that ffmpeg is absent, but
// the only place it said so was the server log (config.go). A missing ffmpeg
// does not fail anything loudly: video compression switches itself off and
// videos are stored as sent, so images keep working and the installation looks
// successful. Nobody finds out until somebody uploads a video, which can be
// weeks later.
//
// This file turns that silence into something an administrator sees on the
// page they are already looking at, and it goes further than the PATH lookup in
// three ways that the lookup structurally cannot cover:
//
//  1. It RUNS the binary. exec.LookPath proves a file exists and carries the
//     executable bit; it does not prove the file starts. A wrong architecture
//     or a missing shared library passes the lookup and fails every upload.
//  2. It checks the ENCODER for the codec actually selected. An ffmpeg built
//     without --enable-libx265 accepts the H.265 setting and then fails at
//     transcode time, which SPEC §4.2 turns into a rejected upload — the
//     sender is refused because of an administrator's configuration.
//  3. It checks the SPOOL directory, which every video and every voice message
//     needs and which nothing else reports on.
//
// What it deliberately does NOT do is change behaviour. A missing encoder is
// shown in red and the upload is still rejected exactly as SPEC §4.2 requires
// and four slices verified. Making the plugin silently store videos as sent
// instead would be a specification change dressed up as a health check.

// healthStatus is the severity of a single check.
type healthStatus string

const (
	// healthOK means the check found what it needs.
	healthOK healthStatus = "ok"
	// healthWarn is worth reading but nothing is broken.
	healthWarn healthStatus = "warn"
	// healthError means a feature is switched off or will fail.
	healthError healthStatus = "error"
)

// healthCheck is one line in the report.
type healthCheck struct {
	// ID is the stable key the webapp and the tests address a check by. It
	// never changes; Label is what a human reads and may be reworded.
	ID string `json:"id"`
	// Label names the check in the administrator's words.
	Label string `json:"label"`
	// Status drives the colour.
	Status healthStatus `json:"status"`
	// Detail is the sentence under the label: what was found, where, and —
	// when something is wrong — what to do about it. An administrator must be
	// able to act on this without reading the source.
	Detail string `json:"detail"`
}

// healthResult is the whole report.
type healthResult struct {
	// Healthy is false when any check is an error. The webapp uses it to
	// decide whether to shout; it is not merely decorative.
	Healthy bool `json:"healthy"`
	// Checks are in display order.
	Checks []healthCheck `json:"checks"`
	// CheckedAt lets an administrator see the answer is fresh rather than a
	// cached copy from before they installed ffmpeg.
	CheckedAt string `json:"checked_at"`
}

// toolRunner executes a media tool and returns its combined output.
//
// It is injected for the same reason lookPath is (ffmpeg.go): the failure
// cases worth testing are "the binary does not start" and "this build has no
// libx265", and neither can be provoked on a machine with a healthy ffmpeg.
// Testing them by breaking the developer's installation is not a test anyone
// would run twice.
type toolRunner func(name string, args ...string) (string, error)

// healthProbeTimeout bounds the two short calls this file makes.
//
// `ffmpeg -version` and `ffmpeg -encoders` are local, sub-second operations.
// The timeout exists for the pathological case — a binary on a hung network
// mount — because this runs on an HTTP request an administrator is waiting on.
const healthProbeTimeout = 5 * time.Second

// runToolCommand is the production toolRunner.
func runToolCommand(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	isolateProcessGroup(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// toolRunnerOrDefault returns the injected runner, or the real one.
func (p *MediaForgePlugin) toolRunnerOrDefault() toolRunner {
	if p.runTool != nil {
		return p.runTool
	}
	return runToolCommand
}

// firstLine trims the output of `-version` down to the line that names it.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

// versionOf runs `<tool> -version` and returns the identifying first line.
func versionOf(run toolRunner, path string) (string, error) {
	out, err := run(path, "-version")
	if err != nil {
		return "", err
	}
	line := firstLine(out)
	if line == "" {
		return "", fmt.Errorf("%s produced no version output", filepath.Base(path))
	}
	return line, nil
}

// checkTool builds the ffmpeg or ffprobe row.
func checkTool(run toolRunner, label, path, reason string) healthCheck {
	c := healthCheck{ID: strings.ToLower(label), Label: label}

	if path == "" {
		c.Status = healthError
		detail := reason
		if detail == "" {
			detail = label + " was not found in the server's PATH"
		}
		// The remedy belongs here rather than in documentation the
		// administrator would have to go and find.
		c.Detail = detail + ". Install it on the server (on Debian or Ubuntu: " +
			"sudo apt install ffmpeg) and reload this page."
		return c
	}

	version, err := versionOf(run, path)
	if err != nil {
		// Present, executable, and still does not run. This is the case the
		// PATH lookup cannot see.
		c.Status = healthError
		c.Detail = fmt.Sprintf(
			"%s was found at %s but could not be run (%v). The file may be for a "+
				"different architecture or missing a shared library.", label, path, err)
		return c
	}

	c.Status = healthOK
	c.Detail = fmt.Sprintf("%s — %s", path, version)
	return c
}

// encoderFor names the ffmpeg encoder the selected codec needs.
func encoderFor(codec string) string {
	if profile, ok := videoProfiles[codec]; ok {
		return profile.videoCodec
	}
	return ""
}

// checkEncoder asks the ffmpeg that is actually installed whether it can
// produce the format the administrator selected.
//
// `ffmpeg -encoders` lists what was compiled in. Debian's ffmpeg carries
// libx264 and libx265; a minimal or self-built one often carries neither, and
// the setting page happily offers all three codecs regardless.
func checkEncoder(run toolRunner, cfg configuration) healthCheck {
	c := healthCheck{ID: "encoder", Label: "Encoder for the selected video format"}

	encoder := encoderFor(cfg.VideoCodec)
	if encoder == "" {
		// sanitiseConfiguration only ever stores a codec from the allowlist,
		// so this is unreachable by configuration. It is handled rather than
		// ignored because a future codec added to the table without a profile
		// should show up here instead of reporting a false OK.
		c.Status = healthWarn
		c.Detail = fmt.Sprintf("no encoder is known for the codec %q", cfg.VideoCodec)
		return c
	}

	if cfg.tools.FFmpegPath == "" {
		c.Status = healthError
		c.Detail = "cannot be checked because ffmpeg was not found."
		return c
	}

	out, err := run(cfg.tools.FFmpegPath, "-hide_banner", "-encoders")
	if err != nil {
		c.Status = healthWarn
		c.Detail = fmt.Sprintf("ffmpeg could not list its encoders (%v), "+
			"so this could not be verified.", err)
		return c
	}

	if !encoderListed(out, encoder) {
		c.Status = healthError
		// Naming the encoder AND the setting is what makes this actionable:
		// the administrator picked "H.265", and the thing that is missing is
		// called "libx265".
		c.Detail = fmt.Sprintf(
			"this ffmpeg has no %s encoder, which the selected video format (%s) needs. "+
				"Videos will be refused with an error until you either install an ffmpeg "+
				"built with %s or choose a different video format below.",
			encoder, cfg.VideoCodec, encoder)
		return c
	}

	c.Status = healthOK
	c.Detail = fmt.Sprintf("%s is available for the selected format (%s).", encoder, cfg.VideoCodec)
	return c
}

// encoderListed reports whether `ffmpeg -encoders` offers the named encoder.
//
// The output is one encoder per line as " V....D libx264  H.264 ...", so a
// plain substring search over the whole blob would also match the description
// text of an unrelated entry. Matching the second field of a line is what makes
// this answer the question that was asked.
//
// The capability column is checked too, and the reason is specific rather than
// defensive. The listing opens with a legend whose lines have the SAME shape as
// an encoder line:
//
//	V..... = Video          <- legend: field 0 "V.....", field 1 "="
//	V....D libx264   H.264  <- encoder: field 0 "V....D", field 1 "libx264"
//
// so field 0 alone does not separate them — both look like flags. What
// separates them is that an encoder name is never "=", and that a real
// capability column ends in a letter or dot rather than being punctuation.
// No codec can be called "=", so this was never reachable through the
// settings; it is tightened because a parser that answers yes to the wrong
// question is a poor foundation for whoever calls it next.
func encoderListed(out, encoder string) bool {
	if !plausibleEncoderName(encoder) {
		return false
	}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == encoder && looksLikeCapabilityFlags(fields[0]) {
			return true
		}
	}
	return false
}

// plausibleEncoderName rejects anything that cannot be an ffmpeg encoder name.
//
// Encoder names are lowercase alphanumerics with underscores; the legend's "="
// and a header like "Encoders:" are neither.
func plausibleEncoderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// looksLikeCapabilityFlags reports whether s is an ffmpeg capability column
// such as "V....D" or "A....D".
func looksLikeCapabilityFlags(s string) bool {
	if len(s) < 2 {
		return false
	}
	// The first character is the media type. Only video and audio encoders can
	// ever be selected here; subtitle and data encoders are not candidates.
	if s[0] != 'V' && s[0] != 'A' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c != '.' && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// minFreeSpoolBytes is the point below which free space is worth a warning.
//
// One 1080p transcode spools the input and writes an output beside it, so the
// working set is roughly twice the upload. A gigabyte is comfortable for the
// default 2 GB upload limit without being alarmist on a small server.
const minFreeSpoolBytes = 1 << 30

// checkSpool verifies the directory every video and voice message needs.
func (p *MediaForgePlugin) checkSpool(cfg configuration) healthCheck {
	c := healthCheck{ID: "spool", Label: "Temporary working directory"}

	dir, err := ensureSpoolDir(cfg.spoolDir)
	if err != nil {
		c.Status = healthError
		c.Detail = fmt.Sprintf("could not be created (%v). Videos and voice "+
			"messages need it; check the permissions on the server's temporary directory.", err)
		return c
	}

	// Creating the directory is not the same as being able to write in it: it
	// may already exist with the wrong owner, which is the common case after a
	// server has run as one user and been moved to another.
	probe, err := os.CreateTemp(dir, "mediaforge-health-*")
	if err != nil {
		c.Status = healthError
		c.Detail = fmt.Sprintf("%s exists but cannot be written to (%v). Videos and "+
			"voice messages will fail until the server can write there.", dir, err)
		return c
	}
	probePath := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probePath)

	free, err := freeBytesOn(dir)
	if err != nil {
		c.Status = healthOK
		c.Detail = fmt.Sprintf("%s — writable (free space could not be determined).", dir)
		return c
	}

	if free < minFreeSpoolBytes {
		c.Status = healthWarn
		c.Detail = fmt.Sprintf("%s — writable, but only %s free. A large video "+
			"transcode needs roughly twice the size of the upload.", dir, humanBytes(free))
		return c
	}

	c.Status = healthOK
	c.Detail = fmt.Sprintf("%s — writable, %s free.", dir, humanBytes(free))
	return c
}

// humanBytes formats a byte count the way an administrator reads it.
func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/float64(1<<20))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// storeSettings reads where the server keeps uploads.
//
// GetConfig is used rather than GetUnsanitizedConfig: this needs the directory
// and the driver name, neither of which is a secret, and asking for the
// unsanitized config to read two public fields would widen the plugin's reach
// for no reason.
func (p *MediaForgePlugin) storeSettings() (fileStoreSettings, string) {
	api := p.API
	if api == nil {
		return fileStoreSettings{}, ""
	}
	cfg := api.GetConfig()
	if cfg == nil {
		return fileStoreSettings{}, ""
	}

	var fs fileStoreSettings
	if cfg.FileSettings.DriverName != nil {
		fs.DriverName = *cfg.FileSettings.DriverName
	}
	if cfg.FileSettings.Directory != nil {
		fs.Directory = *cfg.FileSettings.Directory
	}

	// The server root is where the Mattermost process was started, which is
	// what a relative FileSettings.Directory is relative to. The plugin's own
	// working directory is NOT the same thing and resolving against it would
	// report on a path that does not exist.
	root, err := os.Getwd()
	if err != nil {
		root = ""
	}
	return fs, root
}

// healthReport runs every check against the configuration in force.
func (p *MediaForgePlugin) healthReport() healthResult {
	cfg := p.getConfiguration()
	run := p.toolRunnerOrDefault()

	fs, serverRoot := p.storeSettings()

	checks := []healthCheck{
		checkTool(run, "ffmpeg", cfg.tools.FFmpegPath, cfg.tools.Reason),
		checkTool(run, "ffprobe", cfg.tools.FFprobePath, cfg.tools.Reason),
		checkEncoder(run, cfg),
		p.checkSpool(cfg),
		checkStore(fs, serverRoot, freeBytesOn),
	}

	result := healthResult{
		Healthy:   true,
		Checks:    checks,
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}
	for _, c := range checks {
		if c.Status == healthError {
			result.Healthy = false
		}
	}
	return result
}

// healthPath is the route the System Console section fetches.
const healthPath = "/health"

// serveHealth answers the health route, or reports that it does not own the
// path.
//
// The permission check is the same as the report's and for the same reason,
// sharpened by the security audit Area 4: this answer contains absolute paths on the
// server, the ffmpeg build in use and how much disk is free. That is precisely
// the class of internal detail the audit removed from messages ordinary users
// can see, so a new route must not hand it back out.
func (p *MediaForgePlugin) serveHealth(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSuffix(r.URL.Path, "/") != healthPath {
		return false
	}

	userID := r.Header.Get("Mattermost-User-Id")
	if userID == "" {
		http.Error(w, "please sign in", http.StatusUnauthorized)
		return true
	}
	if p.API == nil || !p.API.HasPermissionTo(userID, model.PermissionManageSystem) {
		http.Error(w, "this information is only available to system administrators",
			http.StatusForbidden)
		return true
	}

	report := p.healthReport()
	encoded, err := json.Marshal(report)
	if err != nil {
		http.Error(w, "the health report could not be prepared", http.StatusInternalServerError)
		return true
	}

	w.Header().Set("Content-Type", "application/json")
	// The whole value of this page is that it tells the truth right now: an
	// administrator installs ffmpeg and reloads to confirm. A cached answer
	// would show them the problem they have just fixed.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(encoded)
	return true
}
