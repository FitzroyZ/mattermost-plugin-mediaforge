package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
)

// The health report is the answer to a defect the cutover found in the
// procedure rather than in the code: detectMediaTools already knows ffmpeg is
// missing, but it only ever said so in the server log (config.go 681, 691).
// An administrator who does not read the log installs the plugin, sees nothing
// wrong, and learns weeks later that no video was ever compressed.
//
// These tests pin what the System Console is allowed to show and, just as
// importantly, who is allowed to see it: a binary path and the free disk space
// are exactly the internals the security audit Area 4 removed from user-facing messages.

// fakeToolRunner returns canned output instead of executing anything, so the
// "ffmpeg is broken" and "encoder missing" cases can be tested on a machine
// where ffmpeg is present and healthy. Running the real binary would make the
// result depend on the developer's own installation.
type fakeToolRunner struct {
	version  string
	encoders string
	err      error
}

func (f fakeToolRunner) run(_ string, args ...string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	for _, a := range args {
		if a == "-encoders" {
			return f.encoders, nil
		}
	}
	return f.version, nil
}

func healthTestPlugin(t *testing.T, tools mediaTools, runner fakeToolRunner, spool string) *MediaForgePlugin {
	t.Helper()
	p := &MediaForgePlugin{}
	cfg := defaultConfiguration()
	cfg.tools = tools
	cfg.spoolDir = spool
	p.configuration.store(cfg)
	p.runTool = runner.run
	return p
}

func TestTheHealthReportNamesTheVersionItFound(t *testing.T) {
	spool := t.TempDir()
	p := healthTestPlugin(t,
		mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"},
		fakeToolRunner{
			version:  "ffmpeg version 7.1.5-0+deb13u1 Copyright (c) 2000-2026 the FFmpeg developers",
			encoders: " V....D libx264 H.264\n V....D libx265 HEVC\n A....D aac AAC\n",
		}, spool)

	report := p.healthReport()

	ffmpeg := findCheck(t, report, "ffmpeg")
	if ffmpeg.Status != healthOK {
		t.Fatalf("ffmpeg should be OK, got %q (%s)", ffmpeg.Status, ffmpeg.Detail)
	}
	// The version is the whole point of the check: detectMediaTools only
	// proves a file exists at that path and is executable. It does not prove
	// the file runs, and it says nothing about which encoders were compiled
	// into it.
	if !strings.Contains(ffmpeg.Detail, "7.1.5") {
		t.Errorf("the detail should name the version that was found, got %q", ffmpeg.Detail)
	}
	if !strings.Contains(ffmpeg.Detail, "/usr/bin/ffmpeg") {
		t.Errorf("the detail should name the path that was found, got %q", ffmpeg.Detail)
	}
}

func TestAMissingFFmpegIsReportedAsAnError(t *testing.T) {
	spool := t.TempDir()
	// Reason is what detectMediaTools produces when the lookup fails.
	p := healthTestPlugin(t,
		mediaTools{Reason: "ffmpeg and ffprobe not found in the server's PATH, " +
			"so video compression is switched off and videos are stored exactly as they were sent"},
		fakeToolRunner{}, spool)

	report := p.healthReport()

	if report.Healthy {
		t.Error("the report must not call itself healthy when ffmpeg is missing")
	}
	ffmpeg := findCheck(t, report, "ffmpeg")
	if ffmpeg.Status != healthError {
		t.Fatalf("a missing ffmpeg is an error, got %q", ffmpeg.Status)
	}
	// An administrator has to be able to act on this without reading the
	// source, so the remedy belongs in the message.
	if !strings.Contains(strings.ToLower(ffmpeg.Detail), "install") {
		t.Errorf("the detail should tell the administrator what to do, got %q", ffmpeg.Detail)
	}
}

func TestAnFFmpegThatDoesNotRunIsReportedAsAnError(t *testing.T) {
	spool := t.TempDir()
	// The file is present and executable — detectMediaTools is happy — but
	// running it fails. A wrong architecture or a broken library does this,
	// and the PATH check alone cannot see it.
	p := healthTestPlugin(t,
		mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"},
		fakeToolRunner{err: os.ErrPermission}, spool)

	report := p.healthReport()

	if report.Healthy {
		t.Error("an ffmpeg that cannot be executed must not read as healthy")
	}
	if got := findCheck(t, report, "ffmpeg").Status; got != healthError {
		t.Errorf("an unrunnable ffmpeg is an error, got %q", got)
	}
}

func TestTheSelectedCodecIsCheckedAgainstTheEncodersThisFFmpegHas(t *testing.T) {
	spool := t.TempDir()
	// A Debian ffmpeg built without --enable-libx265. The plugin accepts the
	// H.265 setting today and the upload fails later, which SPEC §4.2 turns
	// into a REJECTED upload — the user sees a refusal for an administrator's
	// configuration mistake.
	p := healthTestPlugin(t,
		mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"},
		fakeToolRunner{
			version:  "ffmpeg version 7.1.5",
			encoders: " V....D libx264 H.264\n A....D aac AAC\n",
		}, spool)

	cfg := p.getConfiguration()
	cfg.VideoCodec = videoCodecH265
	cfg.tools = mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"}
	cfg.spoolDir = spool
	p.configuration.store(cfg)

	report := p.healthReport()

	enc := findCheck(t, report, "encoder")
	if enc.Status != healthError {
		t.Fatalf("a selected codec with no encoder is an error, got %q (%s)", enc.Status, enc.Detail)
	}
	if !strings.Contains(enc.Detail, "libx265") {
		t.Errorf("the detail should name the encoder that is missing, got %q", enc.Detail)
	}
	// Deliberately NOT changed: the plugin still rejects such uploads exactly
	// as SPEC §4.2 requires and four slices verified. This check makes the
	// cause visible; it does not quietly switch the behaviour.
	if report.Healthy {
		t.Error("the report must not be healthy when the selected codec cannot be produced")
	}
}

func TestAPresentEncoderPasses(t *testing.T) {
	spool := t.TempDir()
	p := healthTestPlugin(t,
		mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"},
		fakeToolRunner{
			version:  "ffmpeg version 7.1.5",
			encoders: " V....D libx264 H.264\n A....D aac AAC\n",
		}, spool)

	if got := findCheck(t, p.healthReport(), "encoder").Status; got != healthOK {
		t.Errorf("libx264 is present, so the check should pass, got %q", got)
	}
}

func TestAnUnwritableSpoolDirectoryIsReported(t *testing.T) {
	// A read-only spool directory means every video and every voice message
	// fails, and nothing in the console says why.
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatalf("preparing the fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; " +
			"TestASpoolDirectoryThatCannotBeCreatedIsReported covers the error path here")
	}

	p := healthTestPlugin(t,
		mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"},
		fakeToolRunner{version: "ffmpeg version 7.1.5", encoders: " V....D libx264 H.264\n"},
		filepath.Join(dir, "spool"))

	if got := findCheck(t, p.healthReport(), "spool").Status; got != healthError {
		t.Errorf("an unwritable spool directory is an error, got %q", got)
	}
}

// TestASpoolDirectoryThatCannotBeCreatedIsReported covers the same failure on a
// machine running the suite as root, where the permission fixture above cannot
// bite. Without it, CI as root would report the spool error path as "skipped"
// and nobody would notice it had never been exercised.
//
// A path under /proc/self/fd/0 cannot be turned into a directory by anyone,
// root included, because the first component is not a directory.
func TestASpoolDirectoryThatCannotBeCreatedIsReported(t *testing.T) {
	p := healthTestPlugin(t,
		mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"},
		fakeToolRunner{version: "ffmpeg version 7.1.5", encoders: " V....D libx264 H.264\n"},
		filepath.Join("/proc/self/fd/0", "spool"))

	c := findCheck(t, p.healthReport(), "spool")
	if c.Status != healthError {
		t.Fatalf("a spool directory that cannot be created is an error, got %q (%s)",
			c.Status, c.Detail)
	}
	if !strings.Contains(strings.ToLower(c.Detail), "videos and voice messages") {
		t.Errorf("the detail should say what stops working, got %q", c.Detail)
	}
}

func TestAWritableSpoolDirectoryReportsItsFreeSpace(t *testing.T) {
	spool := t.TempDir()
	p := healthTestPlugin(t,
		mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"},
		fakeToolRunner{version: "ffmpeg version 7.1.5", encoders: " V....D libx264 H.264\n"},
		spool)

	spoolCheck := findCheck(t, p.healthReport(), "spool")
	if spoolCheck.Status == healthError {
		t.Fatalf("a writable temp dir should not be an error: %s", spoolCheck.Detail)
	}
	if !strings.Contains(spoolCheck.Detail, spool) {
		t.Errorf("the detail should name the directory, got %q", spoolCheck.Detail)
	}
	if !strings.Contains(spoolCheck.Detail, "GB") && !strings.Contains(spoolCheck.Detail, "MB") {
		t.Errorf("the detail should report the free space, got %q", spoolCheck.Detail)
	}
}

// --- the route, and who may read it -----------------------------------------

// The health report names absolute paths on the server and how much disk is
// free. the security audit Area 4 established that such internals do not go to ordinary
// users; a new route is not an exception to that.

func TestTheHealthRouteRequiresASession(t *testing.T) {
	p := healthTestPlugin(t, mediaTools{}, fakeToolRunner{}, t.TempDir())

	rec := httptest.NewRecorder()
	p.ServeHTTP(nil, rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no session must be refused with 401, got %d", rec.Code)
	}
}

func TestTheHealthRouteRefusesAnOrdinaryUser(t *testing.T) {
	p := healthTestPlugin(t, mediaTools{}, fakeToolRunner{}, t.TempDir())
	api := &plugintest.API{}
	api.On("HasPermissionTo", mock.Anything, mock.Anything).Return(false).Maybe()
	p.SetAPI(api)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Mattermost-User-Id", "ordinary-user")
	rec := httptest.NewRecorder()
	p.ServeHTTP(nil, rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("an ordinary user must get 403, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "/usr/bin") {
		t.Error("the refusal must not leak a path")
	}
}

func TestTheHealthRouteAnswersAnAdministratorWithJSON(t *testing.T) {
	spool := t.TempDir()
	p := healthTestPlugin(t,
		mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"},
		fakeToolRunner{version: "ffmpeg version 7.1.5", encoders: " V....D libx264 H.264\n"},
		spool)
	api := &plugintest.API{}
	api.On("HasPermissionTo", mock.Anything, mock.Anything).Return(true).Maybe()
	// The report now reads the server's file-store settings, so the mock has
	// to answer for them. Without this the route panics rather than fails,
	// which is how the missing stub first showed up.
	api.On("GetConfig").Return(serverConfigWithStore("local", t.TempDir())).Maybe()
	p.SetAPI(api)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Mattermost-User-Id", "admin-user")
	rec := httptest.NewRecorder()
	p.ServeHTTP(nil, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("an administrator should get 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var report healthResult
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("the answer should be JSON: %v", err)
	}
	if len(report.Checks) == 0 {
		t.Error("the report should carry at least one check")
	}
	// The figures change as soon as somebody installs ffmpeg, so a cached
	// answer would show an administrator a problem they have already fixed.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("the answer must not be cached, got %q", got)
	}
}

func findCheck(t *testing.T, report healthResult, id string) healthCheck {
	t.Helper()
	for _, c := range report.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("the report has no check %q; it has %d checks", id, len(report.Checks))
	return healthCheck{}
}

// --- the encoder parser, against real ffmpeg output --------------------------

// TestEncoderListedAgainstRealFFmpegOutput checks the parser against what the
// installed ffmpeg actually prints, not a hand-written fixture.
//
// It earned its place: written as a throwaway probe to verify a claim,
// it immediately found that matching only the encoder column treats the legend
// line " V..... = Video" as an encoder called "=". Nothing can select a codec
// named "=", so it was never exploitable — but a fixture written by the same
// person who wrote the parser would have reproduced the same blind spot, and
// this did not.
func TestEncoderListedAgainstRealFFmpegOutput(t *testing.T) {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		t.Skipf("no ffmpeg on this machine: %v", err)
	}
	text := string(out)

	// Present in any ordinary build, and the three the plugin can select.
	for _, enc := range []string{"libx264", "aac"} {
		if !encoderListed(text, enc) {
			t.Errorf("%s should be found in this build", enc)
		}
	}

	// None of these is an available encoder, and each is a different way of
	// being wrong: description text, the legend column, a header, a prefix.
	for _, bogus := range []string{
		"H.264",     // description text on the libx264 line
		"Video",     // the legend line " V..... = Video"
		"=",         // field 1 of every legend line
		"Encoders:", // the header
		"libx26",    // a prefix of a real encoder
		"definitely_not_an_encoder",
	} {
		if encoderListed(text, bogus) {
			t.Errorf("%q must NOT be reported as an available encoder", bogus)
		}
	}
}

// TestTheCapabilityColumnIsWhatDistinguishesAnEncoderLine pins the specific
// shape check, so a later simplification back to "field 1 only" fails here
// rather than silently reporting a legend entry as an encoder.
func TestTheCapabilityColumnIsWhatDistinguishesAnEncoderLine(t *testing.T) {
	listing := strings.Join([]string{
		"Encoders:",
		" V..... = Video",
		" A..... = Audio",
		" ------",
		" V....D libx264               H.264 / AVC (codec h264)",
		" A....D aac                   AAC (Advanced Audio Coding)",
	}, "\n")

	for _, want := range []string{"libx264", "aac"} {
		if !encoderListed(listing, want) {
			t.Errorf("%q is a real encoder line and should match", want)
		}
	}
	for _, reject := range []string{"=", "Video", "Audio"} {
		if encoderListed(listing, reject) {
			t.Errorf("%q comes from the legend, not from an encoder line", reject)
		}
	}
}
