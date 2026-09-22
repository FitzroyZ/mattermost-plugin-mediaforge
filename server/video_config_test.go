package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
)

// loadManifestSettings returns the manifest's settings keyed by their key.
func loadManifestSettings(t *testing.T) map[string]*model.PluginSetting {
	t.Helper()
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatalf("reading the manifest failed: %v", err)
	}
	var manifest model.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the manifest is not valid JSON: %v", err)
	}
	if manifest.SettingsSchema == nil {
		t.Fatal("the manifest declares no settings schema")
	}
	byKey := map[string]*model.PluginSetting{}
	for _, s := range allManifestSettings(manifest.SettingsSchema) {
		byKey[s.Key] = s
	}
	return byKey
}

// TestVideoSettingsValidateLikeTheImageSettings pins the the configuration surface rule on the
// new settings: an invalid value falls back to its own default, leaves the
// other settings alone, and is reported.
//
// Falling back rather than clamping is the deliberate choice from the configuration surface. A
// CRF of 99 becomes 23, not 51 — because 51 is a value nobody chose and the
// console would still be showing 99.
func TestVideoSettingsValidateLikeTheImageSettings(t *testing.T) {
	def := defaultConfiguration()

	cases := []struct {
		name     string
		raw      map[string]any
		check    func(configuration) (any, any)
		warnedOn string
	}{
		{
			name: "a height below the minimum falls back",
			raw:  map[string]any{"MaxVideoHeight": float64(10)},
			check: func(c configuration) (any, any) {
				return c.MaxVideoHeight, def.MaxVideoHeight
			},
			warnedOn: "MaxVideoHeight",
		},
		{
			name: "a height above 8K falls back",
			raw:  map[string]any{"MaxVideoHeight": float64(99999)},
			check: func(c configuration) (any, any) {
				return c.MaxVideoHeight, def.MaxVideoHeight
			},
			warnedOn: "MaxVideoHeight",
		},
		{
			name: "a CRF outside the encoder's range falls back",
			raw:  map[string]any{"VideoCRF": float64(99)},
			check: func(c configuration) (any, any) {
				return c.VideoCRF, def.VideoCRF
			},
			warnedOn: "VideoCRF",
		},
		{
			name: "an unknown codec falls back",
			raw:  map[string]any{"VideoCodec": "vp8"},
			check: func(c configuration) (any, any) {
				return c.VideoCodec, def.VideoCodec
			},
			warnedOn: "VideoCodec",
		},
		{
			name: "a codec of the wrong type falls back",
			raw:  map[string]any{"VideoCodec": float64(264)},
			check: func(c configuration) (any, any) {
				return c.VideoCodec, def.VideoCodec
			},
			warnedOn: "VideoCodec",
		},
		{
			name: "an inaudible audio bitrate falls back",
			raw:  map[string]any{"AudioBitrateKbps": float64(1)},
			check: func(c configuration) (any, any) {
				return c.AudioBitrateKbps, def.AudioBitrateKbps
			},
			warnedOn: "AudioBitrateKbps",
		},
		{
			name: "a timeout of zero falls back",
			raw:  map[string]any{"VideoTimeoutSeconds": float64(0)},
			check: func(c configuration) (any, any) {
				return c.VideoTimeoutSeconds, def.VideoTimeoutSeconds
			},
			warnedOn: "VideoTimeoutSeconds",
		},
		{
			name: "a negative spool cap falls back",
			raw:  map[string]any{"MaxVideoSizeMB": float64(-1)},
			check: func(c configuration) (any, any) {
				return c.MaxVideoSizeMB, def.MaxVideoSizeMB
			},
			warnedOn: "MaxVideoSizeMB",
		},
		{
			name: "a switch that is not a boolean falls back",
			raw:  map[string]any{"EnableVideoCompression": "yes please"},
			check: func(c configuration) (any, any) {
				return c.EnableVideoCompression, def.EnableVideoCompression
			},
			warnedOn: "EnableVideoCompression",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, warnings := sanitiseConfiguration(tc.raw)

			got, want := tc.check(cfg)
			if got != want {
				t.Errorf("the rejected value was applied: got %v, want the default %v", got, want)
			}

			var warned bool
			for _, w := range warnings {
				if w.Setting == tc.warnedOn {
					warned = true
					if !strings.Contains(w.String(), tc.warnedOn) {
						t.Errorf("the warning does not name the setting: %q", w.String())
					}
				}
			}
			if !warned {
				t.Errorf("no warning was produced for %q, so an administrator cannot "+
					"tell why their value has no effect; got %v", tc.warnedOn, warnings)
			}

			// One bad value must not reset the page. Check a neighbouring
			// setting from the other feature is untouched.
			if cfg.JpegQuality != def.JpegQuality {
				t.Errorf("an invalid video value changed JpegQuality to %d", cfg.JpegQuality)
			}
		})
	}
}

// TestValidVideoSettingsAreAccepted is the other half: the accepted range must
// actually be accepted, including its exact edges.
func TestValidVideoSettingsAreAccepted(t *testing.T) {
	raw := map[string]any{
		"EnableVideoCompression": false,
		"MaxVideoHeight":         float64(720),
		"VideoCodec":             "H265", // casing must not matter
		"VideoCRF":               float64(minVideoCRF),
		"AudioBitrateKbps":       float64(maxAudioBitrateKbps),
		"VideoTimeoutSeconds":    float64(minVideoTimeoutSeconds),
		"MaxVideoSizeMB":         float64(minMaxVideoSizeMB),
	}

	cfg, warnings := sanitiseConfiguration(raw)
	if len(warnings) != 0 {
		t.Errorf("valid values produced warnings: %v", warnings)
	}
	if cfg.EnableVideoCompression {
		t.Error("the video switch was not applied")
	}
	if cfg.MaxVideoHeight != 720 {
		t.Errorf("MaxVideoHeight = %d, want 720", cfg.MaxVideoHeight)
	}
	if cfg.VideoCodec != videoCodecH265 {
		t.Errorf("VideoCodec = %q, want %q — the value is matched case-sensitively, "+
			"which the System Console does not guarantee", cfg.VideoCodec, videoCodecH265)
	}
	if cfg.VideoCRF != minVideoCRF {
		t.Errorf("VideoCRF = %d, want %d", cfg.VideoCRF, minVideoCRF)
	}
	if cfg.AudioBitrateKbps != maxAudioBitrateKbps {
		t.Errorf("AudioBitrateKbps = %d, want %d", cfg.AudioBitrateKbps, maxAudioBitrateKbps)
	}
}

// TestNoSettingCanProduceAnUnusableEncoderCommand tests an independent review
// finding rather than trusting either side of it.
//
// The claim was that a MaxVideoHeight of 0 would build the filter
// "scale=-2:min(0,ih)", which ffmpeg rejects, so every upload would be refused
// with "the file may be damaged" and the user would be blamed for a settings
// mistake. The mechanism is real; the premise is not, because sanitisation
// starts from the defaults and a rejected value falls back to its own default
// rather than to the zero value.
//
// It is worth a test regardless: that is a property of sanitiseConfiguration
// which the video code now depends on, and a future refactor returning a bare
// configuration{} on bad input would break it silently.
func TestNoSettingCanProduceAnUnusableEncoderCommand(t *testing.T) {
	hostile := []map[string]any{
		{"MaxVideoHeight": float64(0)},
		{"MaxVideoHeight": float64(-1)},
		{"MaxVideoHeight": "zero"},
		{"MaxVideoHeight": nil},
		{"VideoCRF": float64(-1)},
		{"AudioBitrateKbps": float64(0)},
		{"VideoCodec": ""},
		{"VideoTimeoutSeconds": float64(0)},
		{"MaxVideoSizeMB": float64(0)},
		{}, // nothing configured at all
	}

	for _, raw := range hostile {
		cfg, _ := sanitiseConfiguration(raw)

		if cfg.MaxVideoHeight < minVideoHeight {
			t.Errorf("%v produced MaxVideoHeight=%d, which would build an invalid "+
				"scale filter and refuse every upload", raw, cfg.MaxVideoHeight)
		}
		if cfg.AudioBitrateKbps < minAudioBitrateKbps {
			t.Errorf("%v produced AudioBitrateKbps=%d", raw, cfg.AudioBitrateKbps)
		}
		if cfg.VideoTimeoutSeconds < minVideoTimeoutSeconds {
			t.Errorf("%v produced VideoTimeoutSeconds=%d, so every transcode would "+
				"time out instantly", raw, cfg.VideoTimeoutSeconds)
		}
		if cfg.MaxVideoSizeMB < minMaxVideoSizeMB {
			t.Errorf("%v produced MaxVideoSizeMB=%d", raw, cfg.MaxVideoSizeMB)
		}
		if _, known := videoProfiles[cfg.VideoCodec]; !known {
			t.Errorf("%v produced the unknown codec %q, which has no encoder profile",
				raw, cfg.VideoCodec)
		}

		// And the command it builds must name a real height, not a zero.
		args := encoderArgs(transcodeRequest{
			inputPath: "in.mp4", outputPath: "out.mp4", cfg: cfg,
		})
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "min(0") || strings.Contains(joined, "min(-") {
			t.Errorf("%v built an invalid scale filter: %s", raw, joined)
		}
	}
}

// TestVideoDefaultsMatchTheSpec pins the defaults to SPEC §4.2. Changing one
// is a product decision, and it should not be possible to make it by accident
// while editing the encoder.
func TestVideoDefaultsMatchTheSpec(t *testing.T) {
	def := defaultConfiguration()

	if !def.EnableVideoCompression {
		t.Error("SPEC §4.2 has video compression on by default")
	}
	if def.MaxVideoHeight != 1080 {
		t.Errorf("max resolution = %d, SPEC §4.2 says 1080p", def.MaxVideoHeight)
	}
	if def.VideoCodec != videoCodecH264 {
		t.Errorf("codec = %q, SPEC §4.2 says H.264/MP4", def.VideoCodec)
	}
	if def.VideoCRF != 23 {
		t.Errorf("CRF = %d, SPEC §4.2 says 23", def.VideoCRF)
	}
	if def.AudioBitrateKbps != 128 {
		t.Errorf("audio = %d kbit/s, SPEC §4.2 says AAC 128 kbit/s stereo", def.AudioBitrateKbps)
	}
}

// TestVideoTimeoutStaysInsideTheServersWriteWindow.
//
// The transcode runs while the server's own write timeout is counting down
// (ServiceSettings.WriteTimeout, 300 s by default and on the test instance).
// A default budget at or above that would mean the plugin keeps encoding after
// the server has already abandoned the request — burning every core on a file
// nobody can receive. The default must therefore leave headroom for the
// rejection to be written back.
func TestVideoTimeoutStaysInsideTheServersWriteWindow(t *testing.T) {
	const serverWriteTimeoutSeconds = 300

	def := defaultConfiguration()
	if def.VideoTimeoutSeconds >= serverWriteTimeoutSeconds {
		t.Errorf("the default transcode budget is %d s, which is not inside the server's "+
			"%d s write timeout — the encode would outlive the request",
			def.VideoTimeoutSeconds, serverWriteTimeoutSeconds)
	}
	if serverWriteTimeoutSeconds-def.VideoTimeoutSeconds < 30 {
		t.Errorf("only %d s are left between the transcode budget and the server's write "+
			"timeout; that is not enough to deliver the rejection to the sender",
			serverWriteTimeoutSeconds-def.VideoTimeoutSeconds)
	}
}

// TestVideoIsDisabledWithoutFFmpegWhateverTheSettingSays — the switch alone
// must not be able to enable a feature whose tools are missing.
func TestVideoIsDisabledWithoutFFmpegWhateverTheSettingSays(t *testing.T) {
	cfg := defaultConfiguration()
	cfg.EnableVideoCompression = true

	cfg.tools = mediaTools{}
	if cfg.videoEnabled() {
		t.Error("video reports as enabled with no tools at all")
	}

	cfg.tools = mediaTools{FFmpegPath: "/usr/bin/ffmpeg"}
	if cfg.videoEnabled() {
		t.Error("video reports as enabled with ffprobe missing")
	}

	cfg.tools = mediaTools{FFprobePath: "/usr/bin/ffprobe"}
	if cfg.videoEnabled() {
		t.Error("video reports as enabled with ffmpeg missing")
	}

	cfg.tools = mediaTools{FFmpegPath: "/usr/bin/ffmpeg", FFprobePath: "/usr/bin/ffprobe"}
	if !cfg.videoEnabled() {
		t.Error("video reports as disabled although both tools are present")
	}

	cfg.EnableVideoCompression = false
	if cfg.videoEnabled() {
		t.Error("the administrator's switch was ignored")
	}
}

// TestDetectMediaToolsNamesWhatIsMissing — the message is the feature. An
// administrator seeing "video compression is off" needs to know which program
// to install.
func TestDetectMediaToolsNamesWhatIsMissing(t *testing.T) {
	cases := []struct {
		name     string
		present  map[string]bool
		wantsAll []string
	}{
		{
			name:     "neither is installed",
			present:  map[string]bool{},
			wantsAll: []string{"ffmpeg", "ffprobe"},
		},
		{
			name:     "only ffprobe is missing",
			present:  map[string]bool{"ffmpeg": true},
			wantsAll: []string{"ffprobe"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := detectMediaTools(func(name string) (string, error) {
				if tc.present[name] {
					return "/usr/bin/" + name, nil
				}
				return "", os.ErrNotExist
			})
			if tools.available() {
				t.Fatal("the toolchain reports as available")
			}
			for _, want := range tc.wantsAll {
				if !strings.Contains(tools.Reason, want) {
					t.Errorf("the reason %q does not name %q", tools.Reason, want)
				}
			}
			// It must also say what the consequence is, not just what is
			// missing.
			lower := strings.ToLower(tools.Reason)
			if !strings.Contains(lower, "video") {
				t.Errorf("the reason does not say which feature is affected: %q", tools.Reason)
			}
		})
	}

	tools := detectMediaTools(func(name string) (string, error) { return "/usr/bin/" + name, nil })
	if !tools.available() {
		t.Error("both programs were found but the toolchain reports as unavailable")
	}
	if tools.Reason != "" {
		t.Errorf("a working toolchain still produced a complaint: %q", tools.Reason)
	}
}

// TestCodecHelpTextSaysWhichDevicesPlayTheResult is a requirement of its own,
// not a style check.
//
// SPEC §4.2 offers a codec choice specifically so an administrator can trial a
// newer format. That choice is only informed if the text alone says who can
// play the result and who cannot — an admin who picks H.265 without knowing
// Firefox refuses it discovers the problem from their users, after the videos
// are already stored in that format.
func TestCodecHelpTextSaysWhichDevicesPlayTheResult(t *testing.T) {
	settings := loadManifestSettings(t)
	codec := settings["VideoCodec"]
	if codec == nil {
		t.Fatal("the manifest has no VideoCodec setting")
	}

	help := strings.ToLower(codec.HelpText)

	// Named devices and browsers, not adjectives. "Widely supported" tells an
	// administrator nothing they can check against their own team.
	for _, want := range []string{"iphone", "android", "firefox", "safari", "chrome", "windows"} {
		if !strings.Contains(help, want) {
			t.Errorf("the codec help text never mentions %q, so an administrator cannot "+
				"tell whether their team can play the result", want)
		}
	}

	// It must say what does NOT work, which is the half people skip.
	if !strings.Contains(help, "not play") && !strings.Contains(help, "cannot play") {
		t.Error("the codec help text never states what cannot play the result")
	}

	// The recommendation from SPEC §4.2 has to be visible.
	if !strings.Contains(help, "recommend") && !strings.Contains(strings.ToLower(codec.Options[0].DisplayName), "recommend") {
		t.Error("neither the help text nor the first option marks a recommended format")
	}

	// Each option label must stand on its own: the radio list is what an
	// administrator reads first, and often all they read.
	for _, o := range codec.Options {
		if len(strings.TrimSpace(o.DisplayName)) < 20 {
			t.Errorf("option %q is labelled %q, which says nothing about playability",
				o.Value, o.DisplayName)
		}
	}

	// The options must be exactly the three codecs the code implements.
	got := map[string]bool{}
	for _, o := range codec.Options {
		got[o.Value] = true
	}
	for _, want := range []string{videoCodecH264, videoCodecH265, videoCodecAV1} {
		if !got[want] {
			t.Errorf("the console offers no %q option, so the code path is unreachable", want)
		}
	}
	if len(got) != 3 {
		t.Errorf("the codec setting offers %d options, want exactly 3", len(got))
	}
}

// TestVideoHelpTextsAreWrittenForAdministrators checks the whole video group
// against the wording rules PLAN the configuration surface set out, which this slice inherits.
func TestVideoHelpTextsAreWrittenForAdministrators(t *testing.T) {
	settings := loadManifestSettings(t)

	videoKeys := []string{
		"EnableVideoCompression", "MaxVideoHeight", "VideoCodec",
		"VideoCRF", "AudioBitrateKbps", "VideoTimeoutSeconds", "MaxVideoSizeMB",
	}

	// Words that describe the implementation rather than the effect. A help
	// text containing these is explaining the code to the wrong audience.
	jargon := []string{
		"crf", "bitrate", "libx264", "libx265", "ffmpeg -", "codec_name",
		"transcode", "mux", "yuv420p", "atom", "spool", "goroutine",
	}

	for _, key := range videoKeys {
		t.Run(key, func(t *testing.T) {
			s := settings[key]
			if s == nil {
				t.Fatalf("the manifest has no setting %q", key)
			}
			help := strings.TrimSpace(s.HelpText)
			if len(help) < 80 {
				t.Errorf("help text is only %d characters; it cannot be explaining the "+
					"practical effect: %q", len(help), help)
			}

			lower := strings.ToLower(help)
			for _, j := range jargon {
				if strings.Contains(lower, j) {
					t.Errorf("the help text uses the developer term %q: %q", j, help)
				}
			}

			// The display name must not be the key in disguise.
			if strings.EqualFold(strings.ReplaceAll(s.DisplayName, " ", ""), key) {
				t.Errorf("the display name %q is just the setting key", s.DisplayName)
			}
		})
	}

	// The numeric settings have to state their accepted range, because
	// exceeding it silently falls back to the default and the administrator
	// would otherwise have no way to know why.
	for _, key := range []string{"MaxVideoHeight", "VideoCRF", "AudioBitrateKbps",
		"VideoTimeoutSeconds", "MaxVideoSizeMB"} {
		help := strings.ToLower(settings[key].HelpText)
		if !strings.Contains(help, "allowed") {
			t.Errorf("%s does not state its accepted range, so a rejected value looks "+
				"like a broken setting", key)
		}
	}

	// The on/off switch has to name the dependency: a server without ffmpeg
	// leaves this setting looking broken otherwise.
	enable := strings.ToLower(settings["EnableVideoCompression"].HelpText)
	if !strings.Contains(enable, "ffmpeg") {
		t.Error("the video switch never mentions that ffmpeg must be installed")
	}
	if !strings.Contains(enable, "refus") && !strings.Contains(enable, "reject") {
		t.Error("the video switch never mentions that a failed conversion refuses the upload — " +
			"that is the surprising part and must not be discovered in production")
	}

	// The timeout text must connect to the server setting it interacts with.
	timeout := strings.ToLower(settings["VideoTimeoutSeconds"].HelpText)
	if !strings.Contains(timeout, "write timeout") {
		t.Error("the timeout text does not point at the server's own write timeout, " +
			"which is the setting it has to be coordinated with")
	}
}
