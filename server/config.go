package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Default values for every setting. They are exactly the constants image compression
// compiled in (SPEC §4.1), so an installation that never opens the System
// Console behaves identically after this upgrade. That equivalence is pinned
// byte-for-byte by TestDefaultsReproduceSliceTwoOutputByteForByte.
const (
	// defaultMaxEdgeLength is the longest edge an image may keep. Larger
	// images are scaled down proportionally; smaller ones are never scaled up.
	defaultMaxEdgeLength = 2560
	// defaultJpegQuality is the encoder quality used when re-encoding JPEG.
	defaultJpegQuality = 85
	// defaultMinSizeToProcessKB is the size below which a file is passed
	// through untouched, in kilobytes.
	defaultMinSizeToProcessKB = 512
)

// Default values for the video settings (SPEC §4.2).
const (
	// defaultMaxVideoHeight is the tallest video that is stored unscaled.
	// 1080p is the SPEC §4.2 default and is what phones record.
	defaultMaxVideoHeight = 1080
	// defaultVideoCRF is x264's constant-rate-factor quality. 23 is the
	// encoder's own default and the SPEC §4.2 value.
	defaultVideoCRF = 23
	// defaultAudioBitrateKbps matches SPEC §4.2: AAC 128 kbit/s stereo.
	defaultAudioBitrateKbps = 128
	// defaultVideoTimeoutSeconds bounds one transcode.
	//
	// The number is measured, not guessed. The test instance has
	// ServiceSettings.WriteTimeout = 300, so a transcode that runs longer than
	// that is not slow — it is an upload the server has already given up on,
	// and finishing it would burn cores for a file nobody can receive. 240
	// leaves the server a margin to write the rejection back to the sender.
	// See PLAN §video compression for the timings this is derived from.
	defaultVideoTimeoutSeconds = 240
	// defaultMaxVideoSizeMB caps how much of an upload is written to disk
	// before a transcode. Above it the video is stored as sent: buffering an
	// arbitrarily large file is a resource decision the sender should not pay
	// for with a failed upload.
	defaultMaxVideoSizeMB = 2048
)

// Accepted values of the VideoCodec setting.
const (
	// videoCodecH264 is H.264 video with AAC audio in MP4 — the format that
	// plays on every device anyone is likely to own.
	videoCodecH264 = "h264"
	// videoCodecH265 is H.265/HEVC with AAC in MP4: smaller, less portable.
	videoCodecH265 = "h265"
	// videoCodecAV1 is AV1 with Opus in WebM: smallest and slowest.
	videoCodecAV1 = "av1"
)

// Ranges for the video settings.
const (
	// minVideoHeight is the smallest height that still resembles a video
	// rather than an animated thumbnail.
	minVideoHeight = 144
	// maxVideoHeightLimit is 8K. Above it the setting would do nothing useful
	// on any hardware this plugin is likely to meet.
	maxVideoHeightLimit = 4320
	// CRF is defined by x264/x265 over 0..51. 0 is lossless (enormous files),
	// 51 is unwatchable. The full range is accepted because an administrator
	// tuning for their own footage may legitimately want either end.
	minVideoCRF = 0
	maxVideoCRF = 51
	// Audio bitrates below 32 kbit/s are unintelligible for stereo; above
	// 512 kbit/s AAC gains nothing.
	minAudioBitrateKbps = 32
	maxAudioBitrateKbps = 512
	// A timeout under 10 s cannot finish any real video, and one over an hour
	// is far past every server-side write timeout.
	minVideoTimeoutSeconds = 10
	maxVideoTimeoutSeconds = 3600
	// The spool cap spans one megabyte to one terabyte.
	minMaxVideoSizeMB = 1
	maxMaxVideoSizeMB = 1024 * 1024
)

// The encoder preset is deliberately NOT a setting, and lives with each codec
// profile in ffmpeg.go rather than here.
//
// This is a synchronous transcode with the sender waiting and the server's
// write timeout running, so the speed/efficiency choice is governed by the
// deadline rather than by taste; exposing it would invite an administrator to
// set a value that turns every large upload into a rejection. Measured on the
// 4-core reference container (PLAN §video compression): on a 30-minute clip the 'medium'
// preset needed 498 s — past the server's 300 s window — while 'veryfast'
// stayed well inside it at a comparable file size.

// defaultSizeNoteStyle is the note variant an untouched installation uses.
//
// The small footer, approved on the running instance (PLAN §the size note). It is
// the quietest of the visible variants and the only one that does not write
// into the user's own message.
const defaultSizeNoteStyle = noteStyleFooter

// Accepted values of the ImageMetadata setting.
const (
	// metadataPreserve carries the source file's application segments into the
	// re-encoded image: EXIF, XMP, and the ICC colour profile.
	metadataPreserve = "preserve"
	// metadataStrip writes the re-encoded image with no application segments
	// at all.
	metadataStrip = "strip"
)

// Ranges a value must fall inside to be accepted. Anything outside falls back
// to the default rather than being clamped: clamping would silently apply a
// value the administrator never chose, and the System Console would keep
// showing the rejected one. Falling back is honest and, with the warning in
// the log, diagnosable.
const (
	// minEdgeLength is small but still a usable picture. Below this a photo is
	// a thumbnail, which is not what an upload pipeline should silently produce.
	minEdgeLength = 64
	// maxEdgeLengthLimit is bounded by the decode budget: an image larger than
	// this would be passed through undecoded anyway, so accepting the value
	// would produce a setting that does nothing.
	maxEdgeLengthLimit = 10000
	// maxMinSizeToProcessKB is 1 GB expressed in KB — far above any sane
	// attachment limit, so it rejects only typos.
	maxMinSizeToProcessKB = 1024 * 1024
)

// The bounds a voice duration limit must fall inside.
const (
	// defaultMaxVoiceSeconds is the ten minutes SPEC §4.3 promises.
	defaultMaxVoiceSeconds = 600
	// minMaxVoiceSeconds is short but still usable for a real message. Below
	// this the feature would be a stopwatch rather than a voice message, and
	// a value of zero would read as "no limit" — which is not something this
	// setting offers, because the server has to bound what it converts.
	minMaxVoiceSeconds = 5
	// maxMaxVoiceSecondsLimit is an hour. Beyond it the conversion would
	// outlast voiceTimeout and the recording would be refused anyway, so
	// accepting the value would produce a setting that promises what it
	// cannot deliver.
	maxMaxVoiceSecondsLimit = 3600
)

// configuration is the plugin's settings, already validated.
//
// It is deliberately a plain comparable value type, never a pointer shared
// between goroutines: a reader takes a copy and cannot observe a half-applied
// update. See getConfiguration.
type configuration struct {
	EnableImageCompression bool
	MaxEdgeLength          int
	JpegQuality            int
	MinSizeToProcessKB     int
	ImageMetadata          string

	EnableVideoCompression bool
	MaxVideoHeight         int
	VideoCodec             string
	VideoCRF               int
	AudioBitrateKbps       int
	VideoTimeoutSeconds    int
	MaxVideoSizeMB         int

	// MaxConcurrentVideoJobs and MaxConcurrentImageJobs are the SPEC §4.4
	// concurrency limits.
	MaxConcurrentVideoJobs int
	MaxConcurrentImageJobs int

	// SizeNoteStyle chooses how the size note appears on the post, or
	// switches it off (SPEC §4.4). The fifth option replaces a separate
	// on/off switch, so this single setting is the only place the note is
	// governed from.
	SizeNoteStyle string

	// MarkCompressedFilenames appends `_mm-compressed` to the name of a file
	// the plugin shrank (the file-naming work.2).
	//
	// It is a SETTING rather than a rule because it is a permanent disclosure:
	// unlike the size note, which SizeNoteStyle can switch off afterwards, a
	// filename travels with the file and tells every later recipient that it
	// was altered. some operators want it on this server; another operator may not,
	// and that must not require a code change.
	//
	// It does not govern the OTHER half of the naming rule. A placeholder name
	// — `rn_image_picker_lib_temp_<uuid>` from the mobile app, a bare uuid, a
	// millisecond timestamp — is replaced with a readable one regardless,
	// because nothing there is worth preserving and the result is a file a
	// person can find again in a downloads folder.
	MarkCompressedFilenames bool

	// EnableVoiceMessages switches the record button in the message box on
	// and off (SPEC §4.3).
	//
	// It governs both halves at once, deliberately: the webapp asks the
	// server before it draws the button, so switching this off removes the
	// button AND the conversion together. A button that recorded something
	// the server would then store as WebM would be worse than no button —
	// playable on a desktop, silent on a phone.
	EnableVoiceMessages bool

	// MaxVoiceSeconds is the longest recording the server will convert
	// (SPEC §4.3, default ten minutes).
	//
	// It is enforced on the SERVER rather than only in the browser, because a
	// recording can be uploaded by anything that can post a multipart form —
	// the webapp is one client, not a gate. The browser is told the same
	// number so it can stop a recording in time rather than let somebody
	// speak for eleven minutes and then refuse it.
	MaxVoiceSeconds int

	// VoiceChannels limits recording to named channels, or opens it up
	// everywhere when it is empty (SPEC §4.3: "all / per-channel opt-in").
	//
	// Empty means EVERY channel. That is the default an untouched
	// installation has, and reading it the other way round would switch the
	// feature off for everyone the moment this setting appeared.
	VoiceChannels string

	// TrimVoiceSilence shortens long pauses inside a recording while it is
	// being converted.
	//
	// It answers the most common complaint about voice messages — that they
	// take too long to listen to — at the one point where something can
	// actually be removed rather than merely played faster. Measured with the
	// ffmpeg this plugin already requires: a recording with realistic pauses
	// went from 12.50 s to 9.25 s, and a ten-minute file costs about 7
	// seconds of processing, against 5.8 s for the conversion it rides on.
	//
	// OFF by default, and it is the only feature in this plugin that is.
	// Every other default does the useful thing, because compression is a
	// re-encode: the picture or the sound is reproduced, more cheaply.
	// This one DELETES part of what somebody said, and a pause can carry
	// meaning — hesitation before an awkward answer, a breath, the end of a
	// thought. That is an administrator's decision about their own people's
	// speech, not a default somebody discovers afterwards.
	//
	// It shortens rather than removes, for the same reason (see
	// voiceEncoderArgs).
	TrimVoiceSilence bool

	// videoGate and imageGate enforce those limits. They are pointers to
	// objects owned by the plugin and deliberately SHARED across
	// configuration changes: a new gate per change would let jobs admitted by
	// the old one run alongside jobs admitted by the new one, briefly
	// exceeding both limits. A nil gate means "unlimited", which is what the
	// pipeline's own unit tests use.
	//
	// They are unexported because no administrator types them in; the numbers
	// above are the settings, these are the machinery.
	videoGate *jobGate
	imageGate *jobGate

	// imageQueueWait overrides how long an image may wait for a slot. Zero
	// means the default; the field exists so a test can make the queue-full
	// path reachable without a burst of real uploads.
	imageQueueWait time.Duration

	// tools records where ffmpeg and ffprobe are, or why they are missing. It
	// is resolved once per configuration change rather than per upload, so a
	// server without ffmpeg logs one clear message instead of one per file.
	//
	// It is unexported because it is not a setting: no administrator types it
	// in, and it must not appear in the manifest.
	tools mediaTools

	// spoolDir overrides the directory uploads are buffered in. Empty means
	// the system temp directory, which is the only value production uses; the
	// field exists so a test can watch one directory and prove it ends up
	// empty.
	spoolDir string
}

// defaultConfiguration returns the image compression behaviour.
func defaultConfiguration() configuration {
	return configuration{
		EnableImageCompression: true,
		MaxEdgeLength:          defaultMaxEdgeLength,
		JpegQuality:            defaultJpegQuality,
		MinSizeToProcessKB:     defaultMinSizeToProcessKB,
		ImageMetadata:          metadataPreserve,

		EnableVideoCompression: true,
		MaxVideoHeight:         defaultMaxVideoHeight,
		VideoCodec:             videoCodecH264,
		VideoCRF:               defaultVideoCRF,
		AudioBitrateKbps:       defaultAudioBitrateKbps,
		VideoTimeoutSeconds:    defaultVideoTimeoutSeconds,
		MaxVideoSizeMB:         defaultMaxVideoSizeMB,

		MaxConcurrentVideoJobs: defaultMaxConcurrentVideoJobs,
		MaxConcurrentImageJobs: defaultMaxConcurrentImageJobs,

		SizeNoteStyle: defaultSizeNoteStyle,

		// On by default, decided after trying it on a real server: someone
		// downloading a file should be able to see that it was compressed.
		//
		// This is a disclosure that travels with the file and cannot be taken
		// back later, unlike the size note which SizeNoteStyle can switch off —
		// so it stays a SETTING an operator can turn off, rather than a rule.
		// The default is the recommendation, not a lock.
		MarkCompressedFilenames: true,

		EnableVoiceMessages: true,
		MaxVoiceSeconds:     defaultMaxVoiceSeconds,
		// VoiceChannels stays empty, which means every channel (SPEC §4.3).
	}
}

// videoEnabled reports whether an upload may be transcoded.
//
// Both halves matter: the administrator's switch, and whether the tools are
// actually there. SPEC §5 requires the second one to disable the feature
// quietly rather than fail uploads.
func (c configuration) videoEnabled() bool {
	return c.EnableVideoCompression && c.tools.available()
}

// videoTimeout is the budget for one transcode.
func (c configuration) videoTimeout() time.Duration {
	return time.Duration(c.VideoTimeoutSeconds) * time.Second
}

// maxVideoBytes is the spool cap in bytes.
func (c configuration) maxVideoBytes() int64 {
	return int64(c.MaxVideoSizeMB) * 1024 * 1024
}

// thresholdBytes is the size below which a file is stored untouched.
func (c configuration) thresholdBytes() int {
	return c.MinSizeToProcessKB * 1024
}

// stripMetadata reports whether the re-encoded image must carry no application
// segments.
func (c configuration) stripMetadata() bool {
	return c.ImageMetadata == metadataStrip
}

// configWarning records one rejected setting value.
type configWarning struct {
	// Setting is the manifest key, so an administrator can find the field.
	Setting string
	// Rejected is the value as it arrived, rendered for humans.
	Rejected string
	// Used is the value actually in force.
	Used string
}

func (w configWarning) String() string {
	return fmt.Sprintf(
		"MediaForge: the value %s for setting %q is not valid and was ignored; using the default %s instead",
		w.Rejected, w.Setting, w.Used)
}

// sanitiseConfiguration turns the raw settings object the server hands over
// into a validated configuration.
//
// Two properties matter more than the individual rules:
//
//   - Every rejected value falls back to ITS OWN default and leaves the other
//     settings alone. One typo must not reset the page.
//   - The function never fails. OnConfigurationChange returning an error is
//     the failure mode PLAN the configuration surface calls out, because it turns a typo into an
//     outage; there is nothing this function could report that is worth that.
//
// Keys are matched case-insensitively on purpose. Mattermost stores plugin
// settings in PluginSettings.Plugins[<plugin id>] as a plain JSON object, and
// the values written there do not necessarily carry the manifest's casing. A
// case-sensitive lookup would silently ignore every saved setting and leave the
// plugin permanently on its defaults — a failure that looks exactly like
// "settings do not work" and is invisible in a unit test that uses manifest
// casing.
func sanitiseConfiguration(raw map[string]any) (configuration, []configWarning) {
	cfg := defaultConfiguration()
	var warnings []configWarning

	lower := make(map[string]any, len(raw))
	for k, v := range raw {
		lower[strings.ToLower(k)] = v
	}

	get := func(key string) (any, bool) {
		v, ok := lower[strings.ToLower(key)]
		// A JSON null is indistinguishable from "not configured" for our
		// purposes, and the server does write nulls for cleared fields.
		if !ok || v == nil {
			return nil, false
		}
		return v, true
	}

	warn := func(setting string, rejected any, used string) {
		warnings = append(warnings, configWarning{
			Setting:  setting,
			Rejected: renderValue(rejected),
			Used:     used,
		})
	}

	if v, ok := get("EnableImageCompression"); ok {
		b, valid := v.(bool)
		if valid {
			cfg.EnableImageCompression = b
		} else {
			warn("EnableImageCompression", v, strconv.FormatBool(cfg.EnableImageCompression))
		}
	}

	if v, ok := get("MaxEdgeLength"); ok {
		n, valid := asInt(v)
		if valid && n >= minEdgeLength && n <= maxEdgeLengthLimit {
			cfg.MaxEdgeLength = n
		} else {
			warn("MaxEdgeLength", v, strconv.Itoa(cfg.MaxEdgeLength))
		}
	}

	if v, ok := get("JpegQuality"); ok {
		n, valid := asInt(v)
		if valid && n >= 1 && n <= 100 {
			cfg.JpegQuality = n
		} else {
			warn("JpegQuality", v, strconv.Itoa(cfg.JpegQuality))
		}
	}

	if v, ok := get("MinSizeToProcessKB"); ok {
		n, valid := asInt(v)
		if valid && n >= 0 && n <= maxMinSizeToProcessKB {
			cfg.MinSizeToProcessKB = n
		} else {
			warn("MinSizeToProcessKB", v, strconv.Itoa(cfg.MinSizeToProcessKB))
		}
	}

	if v, ok := get("EnableVideoCompression"); ok {
		b, valid := v.(bool)
		if valid {
			cfg.EnableVideoCompression = b
		} else {
			warn("EnableVideoCompression", v, strconv.FormatBool(cfg.EnableVideoCompression))
		}
	}

	if v, ok := get("MaxVideoHeight"); ok {
		n, valid := asInt(v)
		if valid && n >= minVideoHeight && n <= maxVideoHeightLimit {
			cfg.MaxVideoHeight = n
		} else {
			warn("MaxVideoHeight", v, strconv.Itoa(cfg.MaxVideoHeight))
		}
	}

	if v, ok := get("VideoCodec"); ok {
		s, isString := v.(string)
		codec := strings.ToLower(strings.TrimSpace(s))
		if isString && (codec == videoCodecH264 || codec == videoCodecH265 || codec == videoCodecAV1) {
			cfg.VideoCodec = codec
		} else {
			warn("VideoCodec", v, cfg.VideoCodec)
		}
	}

	if v, ok := get("VideoCRF"); ok {
		n, valid := asInt(v)
		if valid && n >= minVideoCRF && n <= maxVideoCRF {
			cfg.VideoCRF = n
		} else {
			warn("VideoCRF", v, strconv.Itoa(cfg.VideoCRF))
		}
	}

	if v, ok := get("AudioBitrateKbps"); ok {
		n, valid := asInt(v)
		if valid && n >= minAudioBitrateKbps && n <= maxAudioBitrateKbps {
			cfg.AudioBitrateKbps = n
		} else {
			warn("AudioBitrateKbps", v, strconv.Itoa(cfg.AudioBitrateKbps))
		}
	}

	if v, ok := get("VideoTimeoutSeconds"); ok {
		n, valid := asInt(v)
		if valid && n >= minVideoTimeoutSeconds && n <= maxVideoTimeoutSeconds {
			cfg.VideoTimeoutSeconds = n
		} else {
			warn("VideoTimeoutSeconds", v, strconv.Itoa(cfg.VideoTimeoutSeconds))
		}
	}

	if v, ok := get("MaxVideoSizeMB"); ok {
		n, valid := asInt(v)
		if valid && n >= minMaxVideoSizeMB && n <= maxMaxVideoSizeMB {
			cfg.MaxVideoSizeMB = n
		} else {
			warn("MaxVideoSizeMB", v, strconv.Itoa(cfg.MaxVideoSizeMB))
		}
	}

	if v, ok := get("MaxConcurrentVideoJobs"); ok {
		n, valid := asInt(v)
		if valid && n >= 1 && n <= maxConcurrentJobsLimit {
			cfg.MaxConcurrentVideoJobs = n
		} else {
			warn("MaxConcurrentVideoJobs", v, strconv.Itoa(cfg.MaxConcurrentVideoJobs))
		}
	}

	if v, ok := get("MaxConcurrentImageJobs"); ok {
		n, valid := asInt(v)
		if valid && n >= 1 && n <= maxConcurrentJobsLimit {
			cfg.MaxConcurrentImageJobs = n
		} else {
			warn("MaxConcurrentImageJobs", v, strconv.Itoa(cfg.MaxConcurrentImageJobs))
		}
	}

	if v, ok := get("ImageMetadata"); ok {
		s, isString := v.(string)
		mode := strings.ToLower(strings.TrimSpace(s))
		if isString && (mode == metadataPreserve || mode == metadataStrip) {
			cfg.ImageMetadata = mode
		} else {
			warn("ImageMetadata", v, cfg.ImageMetadata)
		}
	}

	if v, ok := get("SizeNoteStyle"); ok {
		s, isString := v.(string)
		style := strings.ToLower(strings.TrimSpace(s))
		switch {
		case isString && (style == noteStyleLine || style == noteStyleCompact ||
			style == noteStyleBlock || style == noteStyleFooter || style == noteStyleNone):
			cfg.SizeNoteStyle = style
		default:
			warn("SizeNoteStyle", v, cfg.SizeNoteStyle)
		}
	}

	if v, ok := get("MarkCompressedFilenames"); ok {
		b, valid := v.(bool)
		if valid {
			cfg.MarkCompressedFilenames = b
		} else {
			warn("MarkCompressedFilenames", v, strconv.FormatBool(cfg.MarkCompressedFilenames))
		}
	}

	if v, ok := get("EnableVoiceMessages"); ok {
		b, valid := v.(bool)
		if valid {
			cfg.EnableVoiceMessages = b
		} else {
			warn("EnableVoiceMessages", v, strconv.FormatBool(cfg.EnableVoiceMessages))
		}
	}

	if v, ok := get("TrimVoiceSilence"); ok {
		b, valid := v.(bool)
		if valid {
			cfg.TrimVoiceSilence = b
		} else {
			warn("TrimVoiceSilence", v, strconv.FormatBool(cfg.TrimVoiceSilence))
		}
	}

	if v, ok := get("MaxVoiceSeconds"); ok {
		n, valid := asInt(v)
		if valid && n >= minMaxVoiceSeconds && n <= maxMaxVoiceSecondsLimit {
			cfg.MaxVoiceSeconds = n
		} else {
			warn("MaxVoiceSeconds", v, strconv.Itoa(cfg.MaxVoiceSeconds))
		}
	}

	if v, ok := get("VoiceChannels"); ok {
		// Any text is acceptable here, including text that names no channel
		// that exists. A typed channel name cannot be validated at this point
		// — the channels are in the database and this function must not reach
		// for it — and rejecting the whole list because one entry is
		// misspelled would switch the feature off in the channels that WERE
		// spelled correctly. An entry that matches nothing simply opens up
		// nothing, which is visible to the administrator as a missing button.
		if s, isString := v.(string); isString {
			cfg.VoiceChannels = s
		} else {
			warn("VoiceChannels", v, strconv.Quote(cfg.VoiceChannels))
		}
	}

	return cfg, warnings
}

// asInt accepts the shapes a number can take on the way from the System
// Console to the plugin.
//
// The configuration crosses the plugin RPC boundary as JSON
// (apiRPCClient.LoadPluginConfiguration unmarshals it), so an integer arrives
// as float64 — or as json.Number if a decoder was configured that way. A
// validator that only accepted int would therefore reject every single saved
// number while looking perfectly correct in a hand-written test.
//
// A fractional value is rejected rather than truncated: 85.5 is not something
// an administrator can mean for a pixel count or a quality step, so silently
// turning it into 85 would hide a mistake.
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

// renderValue formats a rejected value the way an administrator would recognise
// it in the System Console.
//
// Numbers get special treatment because %v on a float64 switches to scientific
// notation for large values: a rejected 1000000 would be reported as "1e+06",
// which is not what the field shows.
func renderValue(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case string:
		return strconv.Quote(n)
	case nil:
		return "empty"
	default:
		return fmt.Sprintf("%v", n)
	}
}

// configurationStore holds the active configuration for concurrent readers.
//
// Uploads run in parallel and the server may call OnConfigurationChange at any
// moment, so this is a genuine data race unless the handover is synchronised.
// An atomic pointer swap is the cheapest correct answer: readers dereference
// once and copy the value, writers publish a fully built replacement. No
// reader can ever observe a partially updated configuration, and the read path
// — which runs on every upload — takes no lock.
type configurationStore struct {
	current atomic.Pointer[configuration]
}

// load returns a copy of the active configuration, or the defaults if the
// server has not called OnConfigurationChange yet.
//
// The fallback is not theoretical tidiness: the zero value of configuration
// reads as "compression disabled, edge length 0, quality 0", which would be a
// silently wrong behaviour rather than an obvious failure.
func (s *configurationStore) load() configuration {
	if c := s.current.Load(); c != nil {
		return *c
	}
	return defaultConfiguration()
}

func (s *configurationStore) store(c configuration) {
	s.current.Store(&c)
}

// getConfiguration returns the settings the next upload must use.
func (p *MediaForgePlugin) getConfiguration() configuration {
	return p.configuration.load()
}

// OnConfigurationChange is invoked by the server whenever the plugin's settings
// may have changed, and once before OnActivate.
//
// It deliberately never returns an error. The server logs a returned error and
// keeps the plugin running, but there is nothing to gain from it here: every
// input this hook can receive has a defined, safe interpretation, and reporting
// a failure would only make an administrator's typo look like a plugin fault.
//
// Because the swap is atomic and the hook is called by the server immediately
// after a setting is saved, a change takes effect on the very next upload — no
// restart, no plugin reload.
func (p *MediaForgePlugin) OnConfigurationChange() error {
	tools := detectMediaTools(p.lookupTool)

	if p.API == nil {
		// Before SetAPI, there is nothing to load. Publishing the defaults
		// keeps the store non-nil from the earliest possible moment.
		cfg := defaultConfiguration()
		cfg.tools = tools
		cfg.videoGate, cfg.imageGate = p.gates(cfg.MaxConcurrentVideoJobs, cfg.MaxConcurrentImageJobs)
		p.configuration.store(cfg)
		return nil
	}

	var raw map[string]any
	if err := p.API.LoadPluginConfiguration(&raw); err != nil {
		// The settings could not be read. Keeping the configuration that is
		// already in force is strictly better than resetting to defaults: a
		// transient API failure must not quietly undo an administrator's
		// deliberate choices mid-flight.
		p.API.LogError(fmt.Sprintf(
			"MediaForge: could not read the plugin configuration (%v); "+
				"continuing with the settings already in force", err))
		return nil
	}

	cfg, warnings := sanitiseConfiguration(raw)
	cfg.tools = tools
	// The gates are resized in place rather than rebuilt, so a limit changed
	// in the System Console governs the very next upload without a restart and
	// without ever allowing more jobs than either limit permits.
	cfg.videoGate, cfg.imageGate = p.gates(cfg.MaxConcurrentVideoJobs, cfg.MaxConcurrentImageJobs)
	for _, w := range warnings {
		p.API.LogWarn(w.String())
	}

	// A missing toolchain is reported as a warning, every time the settings are
	// saved. It has to be loud and repeated: an administrator who switches
	// video compression on and sees nothing happen needs the reason in front of
	// them, and this is the only place the plugin can put it. It is deliberately
	// not an error — an error would disable the plugin and take image
	// compression down with it.
	if cfg.EnableVideoCompression && !tools.available() {
		p.API.LogWarn("MediaForge: video compression is switched on, but " + tools.Reason +
			". Install ffmpeg and ffprobe on the server and save this page again.")
	}

	// The same, for voice messages. An administrator who switches them on and
	// then finds no microphone in the attachment menu has no other way of
	// learning why: the button is simply absent, because the webapp asks the
	// server first and the server answers "not available". Without this line
	// the only visible symptom is a feature that does nothing.
	if cfg.EnableVoiceMessages && !tools.available() {
		p.API.LogWarn("MediaForge: voice messages are switched on, but " + tools.Reason +
			". The record button does not appear until ffmpeg and ffprobe are installed " +
			"on the server. Install them and save this page again.")
	}

	p.configuration.store(cfg)
	p.API.LogDebug(fmt.Sprintf(
		"MediaForge: configuration applied (compression=%v, max edge=%d px, "+
			"JPEG quality=%d, threshold=%d KB, metadata=%s; "+
			"video=%v, max height=%d px, codec=%s, CRF=%d, audio=%d kbit/s, "+
			"timeout=%d s, ffmpeg=%v; at most %d video and %d image job(s) at once)",
		cfg.EnableImageCompression, cfg.MaxEdgeLength, cfg.JpegQuality,
		cfg.MinSizeToProcessKB, cfg.ImageMetadata,
		cfg.videoEnabled(), cfg.MaxVideoHeight, cfg.VideoCodec, cfg.VideoCRF,
		cfg.AudioBitrateKbps, cfg.VideoTimeoutSeconds, tools.available(),
		cfg.MaxConcurrentVideoJobs, cfg.MaxConcurrentImageJobs))
	return nil
}

// lookupTool resolves an external program, honouring a test override.
//
// Production always uses exec.LookPath. The indirection exists so the
// "ffmpeg is not installed" behaviour can be tested on a machine where it is.
func (p *MediaForgePlugin) lookupTool(name string) (string, error) {
	if p.lookPath != nil {
		return p.lookPath(name)
	}
	return exec.LookPath(name)
}
