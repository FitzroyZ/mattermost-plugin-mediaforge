package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// runWith drives the pipeline with an explicit configuration, the way the hook
// does once settings exist.
func runWith(t *testing.T, cfg configuration, input []byte) ([]byte, processOutcome) {
	t.Helper()
	var out bytes.Buffer
	outcome, err := processUpload(&out, bytes.NewReader(input), cfg)
	if err != nil {
		t.Fatalf("processUpload returned an error: %v", err)
	}
	stored := out.Bytes()
	if outcome.bytesWritten != int64(len(stored)) {
		t.Fatalf("outcome.bytesWritten = %d but %d bytes reached the writer",
			outcome.bytesWritten, len(stored))
	}
	return stored, outcome
}

// appSegment builds a JPEG APPn segment with the given marker and payload.
func appSegment(marker byte, payload []byte) []byte {
	seg := []byte{0xFF, marker}
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(payload)+2))
	return append(seg, payload...)
}

// jpegWithAPP1AndICC returns a JPEG carrying both an EXIF APP1 segment and an
// ICC colour profile in APP2.
//
// Both matter for the metadata setting. image compression copies APP1 through APP15
// verbatim, so "remove" is only honest if the ICC profile disappears too — a
// strip path that only deletes APP1 would leave a segment behind and could
// still carry vendor data.
func jpegWithAPP1AndICC(t *testing.T, baseJPEG []byte) (out, exif, icc []byte) {
	t.Helper()
	withEXIF, exifSeg := jpegWithEXIF(t, baseJPEG)

	iccPayload := append([]byte("ICC_PROFILE\x00\x01\x01"), bytes.Repeat([]byte{0x5A}, 64)...)
	iccSeg := appSegment(0xE2, iccPayload)

	out = make([]byte, 0, len(withEXIF)+len(iccSeg))
	out = append(out, withEXIF[:2]...)
	out = append(out, iccSeg...)
	out = append(out, withEXIF[2:]...)
	return out, exifSeg, iccSeg
}

// countAppMarkers walks a JPEG and reports how many APP1..APP15 segments it
// carries, stopping at the start of scan.
func countAppMarkers(t *testing.T, jpegBytes []byte) int {
	t.Helper()
	if len(jpegBytes) < 4 || jpegBytes[0] != 0xFF || jpegBytes[1] != 0xD8 {
		t.Fatalf("not a JPEG: % X", jpegBytes[:min(4, len(jpegBytes))])
	}
	count, i := 0, 2
	for i+4 <= len(jpegBytes) {
		if jpegBytes[i] != 0xFF {
			break
		}
		pos := i
		for pos+1 < len(jpegBytes) && jpegBytes[pos+1] == 0xFF {
			pos++
		}
		if pos+4 > len(jpegBytes) {
			break
		}
		marker := jpegBytes[pos+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i = pos + 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			break
		}
		length := int(binary.BigEndian.Uint16(jpegBytes[pos+2 : pos+4]))
		if length < 2 || pos+2+length > len(jpegBytes) {
			break
		}
		if marker >= 0xE1 && marker <= 0xEF {
			count++
		}
		i = pos + 2 + length
	}
	return count
}

// ---------------------------------------------------------------------------
// Defaults: the untouched installation must not change behaviour
// ---------------------------------------------------------------------------

// TestDefaultConfigurationMatchesTheSliceTwoConstants pins the defaults to the
// values that were compiled in before this slice. SPEC §4.1 names them; PLAN
// the configuration surface requires an untouched installation to behave identically after the
// upgrade.
func TestDefaultConfigurationMatchesTheSliceTwoConstants(t *testing.T) {
	cfg := defaultConfiguration()

	if !cfg.EnableImageCompression {
		t.Error("EnableImageCompression default = false, want true")
	}
	if cfg.MaxEdgeLength != 2560 {
		t.Errorf("MaxEdgeLength default = %d, want 2560", cfg.MaxEdgeLength)
	}
	if cfg.JpegQuality != 85 {
		t.Errorf("JpegQuality default = %d, want 85", cfg.JpegQuality)
	}
	if cfg.MinSizeToProcessKB != 512 {
		t.Errorf("MinSizeToProcessKB default = %d, want 512", cfg.MinSizeToProcessKB)
	}
	if cfg.ImageMetadata != metadataPreserve {
		t.Errorf("ImageMetadata default = %q, want %q", cfg.ImageMetadata, metadataPreserve)
	}
	if got, want := cfg.thresholdBytes(), 512*1024; got != want {
		t.Errorf("thresholdBytes() = %d, want %d", got, want)
	}
	if cfg.stripMetadata() {
		t.Error("stripMetadata() = true by default, want false (metadata is preserved)")
	}
}

// TestDefaultsReproduceSliceTwoOutputByteForByte is the equivalence proof.
//
// The hashes below were produced by running the image compression pipeline (commit
// 38662ff, before any configuration existed) over these exact fixtures. If the
// default configuration path produces the same bytes, an installation that
// never opens the System Console cannot notice the upgrade.
//
// Byte equality — not "roughly the same size" — is what makes this a proof: any
// drift in quality, edge length, threshold or metadata handling changes the
// hash.
func TestDefaultsReproduceSliceTwoOutputByteForByte(t *testing.T) {
	bigBase := encodeJPEG(t, noisyImage(4000, 3000, 4), 95)
	bigEXIF, _ := jpegWithEXIF(t, bigBase)

	cases := []struct {
		name       string
		input      []byte
		wantSHA256 string
		wantSize   int
		reEncoded  bool
	}{
		{
			name:       "oversized jpeg with exif",
			input:      bigEXIF,
			wantSHA256: "02fc418755cd217176f782c212265a1bb320c353e253489242765cb96e23fa30",
			wantSize:   2323783,
			reEncoded:  true,
		},
		{
			name:       "oversized png",
			input:      encodePNGFast(t, flatImage(3000, 2000)),
			wantSHA256: "ed17fa26406402637e347794b86309f4efcad225cc6356be80f6fb1ef3e61d2b",
			wantSize:   16521,
			reEncoded:  true,
		},
		{
			name:       "small jpeg under the threshold",
			input:      encodeJPEG(t, noisyImage(300, 200, 9), 90),
			wantSHA256: "64c7f3f1916b8f35851ca3ef2ee6c5d32cf45ec0e9e807aea2587f5a9ffb5e19",
			wantSize:   47013,
			reEncoded:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, outcome := runWith(t, defaultConfiguration(), tc.input)

			sum := sha256.Sum256(stored)
			if got := hex.EncodeToString(sum[:]); got != tc.wantSHA256 {
				t.Errorf("stored bytes differ from the image compression output.\n"+
					"  sha256 = %s (%d bytes)\n"+
					"  want   = %s (%d bytes)",
					got, len(stored), tc.wantSHA256, tc.wantSize)
			}
			if outcome.reEncoded != tc.reEncoded {
				t.Errorf("reEncoded = %v, want %v", outcome.reEncoded, tc.reEncoded)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Each setting must actually reach the pipeline
// ---------------------------------------------------------------------------

// TestCompressionCanBeSwitchedOffCompletely covers the kill switch. With the
// feature off, even a large, obviously compressible image must be stored
// exactly as it was sent.
func TestCompressionCanBeSwitchedOffCompletely(t *testing.T) {
	input := encodeJPEG(t, noisyImage(4000, 3000, 11), 95)

	cfg := defaultConfiguration()
	cfg.EnableImageCompression = false

	stored, outcome := runWith(t, cfg, input)

	if outcome.reEncoded {
		t.Error("outcome reports a re-encode although compression is switched off")
	}
	if !bytes.Equal(stored, input) {
		t.Errorf("stored bytes differ although compression is off: %d in, %d out",
			len(input), len(stored))
	}

	// Control: the same fixture must be compressed with the default config,
	// otherwise the assertion above proves nothing.
	if _, on := runWith(t, defaultConfiguration(), input); !on.reEncoded {
		t.Fatal("the fixture is not compressed even with compression enabled; the test is vacuous")
	}
}

// TestMaxEdgeLengthSettingChangesTheStoredGeometry proves the edge limit is
// read from configuration rather than a constant.
func TestMaxEdgeLengthSettingChangesTheStoredGeometry(t *testing.T) {
	input := encodeJPEG(t, noisyImage(4000, 3000, 12), 95)

	for _, edge := range []int{800, 1600} {
		t.Run(fmt.Sprintf("edge_%d", edge), func(t *testing.T) {
			cfg := defaultConfiguration()
			cfg.MaxEdgeLength = edge

			stored, outcome := runWith(t, cfg, input)
			if !outcome.reEncoded {
				t.Fatal("outcome reports no re-encode")
			}
			w, h, format := decodeBounds(t, stored)
			if format != "jpeg" {
				t.Errorf("stored format = %q, want jpeg", format)
			}
			if w != edge {
				t.Errorf("stored long edge = %d, want exactly %d", w, edge)
			}
			if h > edge {
				t.Errorf("stored height %d exceeds the %d px limit", h, edge)
			}
			if outcome.width != w || outcome.height != h {
				t.Errorf("outcome reports %dx%d but the stored file is %dx%d",
					outcome.width, outcome.height, w, h)
			}
		})
	}
}

// TestJpegQualitySettingChangesTheStoredBytes proves the quality setting
// reaches the encoder: a low quality must produce a markedly smaller file than
// a high one for the same input and the same geometry.
func TestJpegQualitySettingChangesTheStoredBytes(t *testing.T) {
	input := encodeJPEG(t, noisyImage(4000, 3000, 13), 95)

	low := defaultConfiguration()
	low.JpegQuality = 20
	high := defaultConfiguration()
	high.JpegQuality = 95

	storedLow, _ := runWith(t, low, input)
	storedHigh, _ := runWith(t, high, input)

	if len(storedLow) >= len(storedHigh) {
		t.Errorf("quality 20 produced %d bytes, quality 95 produced %d; "+
			"lower quality must produce a smaller file",
			len(storedLow), len(storedHigh))
	}

	// Both must still be valid JPEGs of the same size — only the quality
	// changed.
	wl, hl, _ := decodeBounds(t, storedLow)
	wh, hh, _ := decodeBounds(t, storedHigh)
	if wl != wh || hl != hh {
		t.Errorf("geometry differs between qualities: %dx%d vs %dx%d", wl, hl, wh, hh)
	}
}

// TestMinSizeToProcessSettingIsHonoured covers both directions of the
// threshold: lowering it pulls a previously untouched file into the pipeline,
// raising it pushes a previously compressed file out of it.
func TestMinSizeToProcessSettingIsHonoured(t *testing.T) {
	small := encodeJPEG(t, noisyImage(500, 400, 14), 90) // well under 512 KB
	big := encodeJPEG(t, noisyImage(4000, 3000, 15), 95) // well over 512 KB

	if len(small) >= 512*1024 {
		t.Fatalf("the small fixture is %d bytes, not under the default threshold", len(small))
	}
	if len(big) <= 512*1024 {
		t.Fatalf("the big fixture is %d bytes, not over the default threshold", len(big))
	}

	t.Run("lowering the threshold pulls a small file in", func(t *testing.T) {
		// Control first: untouched at the default threshold.
		if _, outcome := runWith(t, defaultConfiguration(), small); outcome.reEncoded {
			t.Fatal("the small fixture is already compressed at the default threshold")
		}

		cfg := defaultConfiguration()
		cfg.MinSizeToProcessKB = 1
		stored, outcome := runWith(t, cfg, small)
		if !outcome.reEncoded {
			t.Fatalf("a %d byte file was not processed at a 1 KB threshold", len(small))
		}
		if _, _, format := decodeBounds(t, stored); format != "jpeg" {
			t.Errorf("stored format = %q, want jpeg", format)
		}
	})

	t.Run("raising the threshold pushes a big file out", func(t *testing.T) {
		cfg := defaultConfiguration()
		cfg.MinSizeToProcessKB = 64 * 1024 // 64 MB
		stored, outcome := runWith(t, cfg, big)
		if outcome.reEncoded {
			t.Error("a file below the raised threshold was still re-encoded")
		}
		if !bytes.Equal(stored, big) {
			t.Errorf("a file below the raised threshold was modified: %d in, %d out",
				len(big), len(stored))
		}
	})

	t.Run("a zero threshold still compresses", func(t *testing.T) {
		cfg := defaultConfiguration()
		cfg.MinSizeToProcessKB = 0
		if _, outcome := runWith(t, cfg, small); !outcome.reEncoded {
			t.Error("a zero threshold left a compressible image untouched")
		}
	})
}

// TestImageMetadataSettingControlsEveryAppSegment is the privacy-relevant one.
//
// image compression copies APP1..APP15 across, which includes the ICC colour profile in
// APP2. "Remove" therefore has to mean all of them: a strip path that only
// deleted APP1 would still hand vendor blocks to the recipient while the
// System Console claims the data is gone.
func TestImageMetadataSettingControlsEveryAppSegment(t *testing.T) {
	base := encodeJPEG(t, noisyImage(4000, 3000, 16), 95)
	input, exif, icc := jpegWithAPP1AndICC(t, base)

	if got := countAppMarkers(t, input); got != 2 {
		t.Fatalf("the fixture carries %d APP1..APP15 segments, want 2", got)
	}

	t.Run("preserve keeps them", func(t *testing.T) {
		cfg := defaultConfiguration()
		cfg.ImageMetadata = metadataPreserve

		stored, outcome := runWith(t, cfg, input)
		if !outcome.reEncoded {
			t.Fatal("outcome reports no re-encode; the metadata test would be vacuous")
		}
		if !bytes.Contains(stored, exif) {
			t.Error("the EXIF APP1 segment did not survive with metadata set to preserve")
		}
		if !bytes.Contains(stored, icc) {
			t.Error("the ICC APP2 segment did not survive with metadata set to preserve")
		}
		if got := countAppMarkers(t, stored); got != 2 {
			t.Errorf("stored file carries %d APP1..APP15 segments, want 2", got)
		}
	})

	t.Run("strip removes all of them", func(t *testing.T) {
		cfg := defaultConfiguration()
		cfg.ImageMetadata = metadataStrip

		stored, outcome := runWith(t, cfg, input)
		if !outcome.reEncoded {
			t.Fatal("outcome reports no re-encode; the metadata test would be vacuous")
		}
		if bytes.Contains(stored, exif) {
			t.Error("the EXIF APP1 segment is still present although metadata is set to remove")
		}
		if bytes.Contains(stored, icc) {
			t.Error("the ICC APP2 segment is still present although metadata is set to remove")
		}
		if got := countAppMarkers(t, stored); got != 0 {
			t.Errorf("stored file still carries %d APP1..APP15 segments, want 0", got)
		}

		// Stripping must not damage the image.
		w, h, format := decodeBounds(t, stored)
		if format != "jpeg" || w != 2560 || h != 1920 {
			t.Errorf("stored image = %dx%d %s, want 2560x1920 jpeg", w, h, format)
		}
	})
}

// TestPNGMetadataIsAlwaysDroppedWhicheverModeIsSet backs a claim the System
// Console makes.
//
// The metadata setting only reaches the JPEG path, because Go's PNG encoder
// writes no ancillary chunks at all — a re-encoded PNG therefore never carries
// a tEXt comment or an eXIf block, whichever option the administrator picked.
// The help text says so; this test is what keeps that sentence true.
func TestPNGMetadataIsAlwaysDroppedWhicheverModeIsSet(t *testing.T) {
	src := encodePNGFast(t, flatImage(3000, 2000))

	// Splice a tEXt chunk in after IHDR (signature 8 bytes + IHDR 25 bytes).
	const afterIHDR = 8 + 25
	payload := append([]byte("tEXt"), []byte("Comment\x00taken at home")...)
	var withText []byte
	withText = append(withText, src[:afterIHDR]...)
	withText = binary.BigEndian.AppendUint32(withText, uint32(len(payload)-4))
	withText = append(withText, payload...)
	withText = binary.BigEndian.AppendUint32(withText, crc32.ChecksumIEEE(payload))
	withText = append(withText, src[afterIHDR:]...)

	if !bytes.Contains(withText, []byte("taken at home")) {
		t.Fatal("the fixture does not carry the comment it is supposed to test")
	}

	for _, mode := range []string{metadataPreserve, metadataStrip} {
		t.Run(mode, func(t *testing.T) {
			cfg := defaultConfiguration()
			cfg.ImageMetadata = mode

			stored, outcome := runWith(t, cfg, withText)
			if !outcome.reEncoded {
				t.Fatalf("the PNG was not re-encoded, so the test is vacuous "+
					"(%d bytes in, %d out)", len(withText), len(stored))
			}
			if bytes.Contains(stored, []byte("taken at home")) {
				t.Errorf("the re-encoded PNG still carries the tEXt comment at %q; "+
					"the help text promises PNG metadata is always dropped", mode)
			}
			if _, _, format := decodeBounds(t, stored); format != "png" {
				t.Errorf("stored format = %q, want png", format)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Validation: bad input falls back, never takes the plugin down
// ---------------------------------------------------------------------------

// TestInvalidValuesFallBackToTheDefaultAndAreReported walks every setting's
// failure modes. A rejected value must leave that one setting at its default,
// must not disturb the others, and must produce a warning naming the setting,
// the rejected value and the value actually used — PLAN the configuration surface, "Validation
// behaviour".
func TestInvalidValuesFallBackToTheDefaultAndAreReported(t *testing.T) {
	def := defaultConfiguration()

	cases := []struct {
		name        string
		raw         map[string]any
		wantSetting string
		wantIn      []string // substrings the warning must contain
		check       func(t *testing.T, cfg configuration)
	}{
		{
			name:        "quality zero",
			raw:         map[string]any{"JpegQuality": float64(0)},
			wantSetting: "JpegQuality",
			wantIn:      []string{"0", "85"},
			check:       func(t *testing.T, c configuration) { mustEqualInt(t, "JpegQuality", c.JpegQuality, def.JpegQuality) },
		},
		{
			name:        "quality above 100",
			raw:         map[string]any{"JpegQuality": float64(140)},
			wantSetting: "JpegQuality",
			wantIn:      []string{"140", "85"},
			check:       func(t *testing.T, c configuration) { mustEqualInt(t, "JpegQuality", c.JpegQuality, def.JpegQuality) },
		},
		{
			name:        "negative edge length",
			raw:         map[string]any{"MaxEdgeLength": float64(-10)},
			wantSetting: "MaxEdgeLength",
			wantIn:      []string{"-10", "2560"},
			check: func(t *testing.T, c configuration) {
				mustEqualInt(t, "MaxEdgeLength", c.MaxEdgeLength, def.MaxEdgeLength)
			},
		},
		{
			name:        "absurd edge length",
			raw:         map[string]any{"MaxEdgeLength": float64(999999)},
			wantSetting: "MaxEdgeLength",
			wantIn:      []string{"999999", "2560"},
			check: func(t *testing.T, c configuration) {
				mustEqualInt(t, "MaxEdgeLength", c.MaxEdgeLength, def.MaxEdgeLength)
			},
		},
		{
			name:        "negative threshold",
			raw:         map[string]any{"MinSizeToProcessKB": float64(-1)},
			wantSetting: "MinSizeToProcessKB",
			wantIn:      []string{"-1", "512"},
			check: func(t *testing.T, c configuration) {
				mustEqualInt(t, "MinSizeToProcessKB", c.MinSizeToProcessKB, def.MinSizeToProcessKB)
			},
		},
		{
			name:        "unknown metadata mode",
			raw:         map[string]any{"ImageMetadata": "shred"},
			wantSetting: "ImageMetadata",
			wantIn:      []string{"shred", "preserve"},
			check: func(t *testing.T, c configuration) {
				mustEqualString(t, "ImageMetadata", c.ImageMetadata, def.ImageMetadata)
			},
		},
		{
			name:        "wrong type for a number",
			raw:         map[string]any{"MaxEdgeLength": "quite large"},
			wantSetting: "MaxEdgeLength",
			wantIn:      []string{"quite large", "2560"},
			check: func(t *testing.T, c configuration) {
				mustEqualInt(t, "MaxEdgeLength", c.MaxEdgeLength, def.MaxEdgeLength)
			},
		},
		{
			name:        "wrong type for the switch",
			raw:         map[string]any{"EnableImageCompression": "yes please"},
			wantSetting: "EnableImageCompression",
			wantIn:      []string{"yes please", "true"},
			check: func(t *testing.T, c configuration) {
				if !c.EnableImageCompression {
					t.Error("EnableImageCompression = false, want the default true")
				}
			},
		},
		{
			name:        "fractional number",
			raw:         map[string]any{"JpegQuality": 85.5},
			wantSetting: "JpegQuality",
			wantIn:      []string{"85.5", "85"},
			check:       func(t *testing.T, c configuration) { mustEqualInt(t, "JpegQuality", c.JpegQuality, def.JpegQuality) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, warnings := sanitiseConfiguration(tc.raw)

			if len(warnings) != 1 {
				t.Fatalf("got %d warnings, want exactly 1: %v", len(warnings), warnings)
			}
			w := warnings[0]
			if w.Setting != tc.wantSetting {
				t.Errorf("warning names %q, want %q", w.Setting, tc.wantSetting)
			}
			text := w.String()
			for _, want := range tc.wantIn {
				if !strings.Contains(text, want) {
					t.Errorf("warning %q does not mention %q", text, want)
				}
			}
			if !strings.Contains(text, tc.wantSetting) {
				t.Errorf("warning %q does not name the setting", text)
			}
			tc.check(t, cfg)

			// One bad value must not disturb the other settings.
			assertOtherSettingsAreDefault(t, cfg, tc.wantSetting)
		})
	}
}

func mustEqualInt(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want the default %d", name, got, want)
	}
}

func mustEqualString(t *testing.T, name string, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want the default %q", name, got, want)
	}
}

func assertOtherSettingsAreDefault(t *testing.T, cfg configuration, except string) {
	t.Helper()
	def := defaultConfiguration()
	if except != "EnableImageCompression" && cfg.EnableImageCompression != def.EnableImageCompression {
		t.Errorf("EnableImageCompression = %v, want %v", cfg.EnableImageCompression, def.EnableImageCompression)
	}
	if except != "MaxEdgeLength" && cfg.MaxEdgeLength != def.MaxEdgeLength {
		t.Errorf("MaxEdgeLength = %d, want %d", cfg.MaxEdgeLength, def.MaxEdgeLength)
	}
	if except != "JpegQuality" && cfg.JpegQuality != def.JpegQuality {
		t.Errorf("JpegQuality = %d, want %d", cfg.JpegQuality, def.JpegQuality)
	}
	if except != "MinSizeToProcessKB" && cfg.MinSizeToProcessKB != def.MinSizeToProcessKB {
		t.Errorf("MinSizeToProcessKB = %d, want %d", cfg.MinSizeToProcessKB, def.MinSizeToProcessKB)
	}
	if except != "ImageMetadata" && cfg.ImageMetadata != def.ImageMetadata {
		t.Errorf("ImageMetadata = %q, want %q", cfg.ImageMetadata, def.ImageMetadata)
	}
}

// TestValidValuesProduceNoWarnings guards against a validator that cries wolf.
func TestValidValuesProduceNoWarnings(t *testing.T) {
	raw := map[string]any{
		"EnableImageCompression": false,
		"MaxEdgeLength":          float64(1280),
		"JpegQuality":            float64(70),
		"MinSizeToProcessKB":     float64(0),
		"ImageMetadata":          "strip",
	}
	cfg, warnings := sanitiseConfiguration(raw)
	if len(warnings) != 0 {
		t.Errorf("valid configuration produced warnings: %v", warnings)
	}
	if cfg.EnableImageCompression || cfg.MaxEdgeLength != 1280 || cfg.JpegQuality != 70 ||
		cfg.MinSizeToProcessKB != 0 || cfg.ImageMetadata != metadataStrip {
		t.Errorf("valid configuration was not applied: %+v", cfg)
	}
}

// TestMissingKeysUseDefaultsSilently covers the fresh install: the server hands
// the plugin an empty object until an administrator saves the page for the
// first time. That is not an error and must not be logged as one.
func TestMissingKeysUseDefaultsSilently(t *testing.T) {
	for _, raw := range []map[string]any{nil, {}} {
		cfg, warnings := sanitiseConfiguration(raw)
		if len(warnings) != 0 {
			t.Errorf("empty configuration produced warnings: %v", warnings)
		}
		if cfg != defaultConfiguration() {
			t.Errorf("empty configuration = %+v, want the defaults %+v", cfg, defaultConfiguration())
		}
	}
}

// TestConfigurationKeysAreMatchedCaseInsensitively reflects how the value
// actually arrives. Mattermost stores plugin settings in
// PluginSettings.Plugins[<id>] as a plain JSON object, and the System Console
// writes the keys in lower case regardless of the casing in the manifest.
// Matching case-sensitively would therefore silently ignore every saved
// setting — a failure that unit tests using manifest casing would never catch.
func TestConfigurationKeysAreMatchedCaseInsensitively(t *testing.T) {
	raw := map[string]any{
		"enableimagecompression": false,
		"maxedgelength":          float64(1024),
		"jpegquality":            float64(60),
		"minsizetoprocesskb":     float64(64),
		"imagemetadata":          "STRIP",
	}
	cfg, warnings := sanitiseConfiguration(raw)
	if len(warnings) != 0 {
		t.Errorf("lower-cased keys produced warnings: %v", warnings)
	}
	want := defaultConfiguration()
	want.EnableImageCompression = false
	want.MaxEdgeLength = 1024
	want.JpegQuality = 60
	want.MinSizeToProcessKB = 64
	want.ImageMetadata = metadataStrip
	if settingsOnly(cfg) != settingsOnly(want) {
		t.Errorf("configuration = %+v, want %+v", cfg, want)
	}
}

// TestJSONNumbersFromTheServerAreAccepted pins the wire format. The plugin API
// hands the configuration over as JSON, so every number arrives as a float64
// (or a json.Number if the decoder is configured that way). A validator that
// only accepted int would reject every single saved number.
func TestJSONNumbersFromTheServerAreAccepted(t *testing.T) {
	var raw map[string]any
	body := `{"EnableImageCompression":true,"MaxEdgeLength":1920,"JpegQuality":75,` +
		`"MinSizeToProcessKB":256,"ImageMetadata":"strip"}`
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("unmarshalling the fixture failed: %v", err)
	}

	cfg, warnings := sanitiseConfiguration(raw)
	if len(warnings) != 0 {
		t.Errorf("a well-formed configuration produced warnings: %v", warnings)
	}
	want := defaultConfiguration()
	want.MaxEdgeLength = 1920
	want.JpegQuality = 75
	want.MinSizeToProcessKB = 256
	want.ImageMetadata = metadataStrip
	if settingsOnly(cfg) != settingsOnly(want) {
		t.Errorf("configuration = %+v, want %+v", cfg, want)
	}
}

// ---------------------------------------------------------------------------
// OnConfigurationChange: never disable the plugin, never race
// ---------------------------------------------------------------------------

// TestOnConfigurationChangeAppliesValidSettings is the "without a restart"
// promise in unit form: after the hook runs, the next upload sees the new
// values.
func TestOnConfigurationChangeAppliesValidSettings(t *testing.T) {
	api := &plugintest.API{}
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		dest := args.Get(0).(*map[string]any)
		*dest = map[string]any{
			"EnableImageCompression": true,
			"MaxEdgeLength":          float64(640),
			"JpegQuality":            float64(50),
			"MinSizeToProcessKB":     float64(1),
			"ImageMetadata":          "strip",
		}
	}).Return(nil)
	api.On("LogInfo", mock.Anything, mock.Anything).Maybe().Return()
	api.On("LogDebug", mock.Anything, mock.Anything).Maybe().Return()
	// On a machine without ffmpeg — CI runners have none — the hook warns
	// that video and voice are unavailable. That warning is correct
	// behaviour, and a mock that forbids it fails the test for the
	// environment rather than for the code.
	api.On("LogWarn", mock.Anything, mock.Anything).Maybe().Return()

	p := &MediaForgePlugin{}
	p.SetAPI(api)

	if err := p.OnConfigurationChange(); err != nil {
		t.Fatalf("OnConfigurationChange returned an error: %v", err)
	}

	got := p.getConfiguration()
	want := defaultConfiguration()
	want.MaxEdgeLength = 640
	want.JpegQuality = 50
	want.MinSizeToProcessKB = 1
	want.ImageMetadata = metadataStrip
	if settingsOnly(got) != settingsOnly(want) {
		t.Errorf("active configuration = %+v, want %+v", got, want)
	}
}

// settingsOnly drops the fields that are not administrator settings, so a
// comparison is about the configuration surface rather than about what happens
// to be installed on the machine running the test.
//
// tools is resolved from the server's PATH and spoolDir is a test hook; the two
// gates are the plugin's own machinery, attached by OnConfigurationChange and
// identified by pointer. None of them is something the System Console can set,
// and none should make an assertion about validation pass or fail — the
// numbers that drive the gates, MaxConcurrentVideoJobs and
// MaxConcurrentImageJobs, are settings and are deliberately left in.
func settingsOnly(c configuration) configuration {
	c.tools = mediaTools{}
	c.spoolDir = ""
	c.videoGate = nil
	c.imageGate = nil
	c.imageQueueWait = 0
	return c
}

// TestOnConfigurationChangeNeverDisablesThePlugin is the outage guard. Returning
// an error from this hook is logged by the server and the plugin keeps running,
// but the plugin must additionally keep serving uploads with usable values —
// a typo in one field must not stop compression or, worse, produce a zeroed
// configuration that silently changes behaviour.
func TestOnConfigurationChangeNeverDisablesThePlugin(t *testing.T) {
	cases := []struct {
		name  string
		setup func(api *plugintest.API)
	}{
		{
			name: "nonsense values",
			setup: func(api *plugintest.API) {
				api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
					dest := args.Get(0).(*map[string]any)
					*dest = map[string]any{
						"MaxEdgeLength":      "huge",
						"JpegQuality":        float64(-3),
						"MinSizeToProcessKB": "lots",
						"ImageMetadata":      "burn it",
					}
				}).Return(nil)
			},
		},
		{
			name: "the API itself fails",
			setup: func(api *plugintest.API) {
				api.On("LoadPluginConfiguration", mock.Anything).
					Return(model.NewAppError("LoadPluginConfiguration", "boom", nil, "", 500))
			},
		},
		{
			name: "nothing configured yet",
			setup: func(api *plugintest.API) {
				api.On("LoadPluginConfiguration", mock.Anything).Return(nil)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &plugintest.API{}
			tc.setup(api)
			api.On("LogWarn", mock.Anything, mock.Anything).Maybe().Return()
			api.On("LogError", mock.Anything, mock.Anything).Maybe().Return()
			api.On("LogInfo", mock.Anything, mock.Anything).Maybe().Return()
			api.On("LogDebug", mock.Anything, mock.Anything).Maybe().Return()

			p := &MediaForgePlugin{}
			p.SetAPI(api)

			if err := p.OnConfigurationChange(); err != nil {
				t.Fatalf("OnConfigurationChange returned %v; returning an error here "+
					"turns an administrator's typo into a broken plugin", err)
			}
			if got := settingsOnly(p.getConfiguration()); got != defaultConfiguration() {
				t.Errorf("active configuration = %+v, want the defaults %+v",
					got, defaultConfiguration())
			}
		})
	}
}

// TestInvalidValuesAreLoggedAsWarnings proves the administrator can find out
// why a setting did not take effect. Silence would leave them staring at a
// value the console shows but the plugin ignores.
func TestInvalidValuesAreLoggedAsWarnings(t *testing.T) {
	var logged []string
	api := &plugintest.API{}
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		dest := args.Get(0).(*map[string]any)
		*dest = map[string]any{"JpegQuality": float64(0)}
	}).Return(nil)
	api.On("LogWarn", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		logged = append(logged, fmt.Sprint(args...))
	}).Return()
	api.On("LogInfo", mock.Anything, mock.Anything).Maybe().Return()
	api.On("LogDebug", mock.Anything, mock.Anything).Maybe().Return()
	// On a machine without ffmpeg — CI runners have none — the hook warns
	// that video and voice are unavailable. That warning is correct
	// behaviour, and a mock that forbids it fails the test for the
	// environment rather than for the code.
	api.On("LogWarn", mock.Anything, mock.Anything).Maybe().Return()

	p := &MediaForgePlugin{}
	p.SetAPI(api)
	if err := p.OnConfigurationChange(); err != nil {
		t.Fatalf("OnConfigurationChange returned an error: %v", err)
	}

	if len(logged) == 0 {
		t.Fatal("an invalid setting was accepted without any log entry")
	}
	joined := strings.Join(logged, "\n")
	for _, want := range []string{"JpegQuality", "0", "85"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log %q does not mention %q", joined, want)
		}
	}
}

// TestConfigurationReadIsSafeDuringConcurrentUploads is the concurrency
// requirement. Uploads run in parallel and the server may call
// OnConfigurationChange at any moment, so the read path must not race with the
// write path. Meaningful under -race, which CI runs.
func TestConfigurationReadIsSafeDuringConcurrentUploads(t *testing.T) {
	api := &plugintest.API{}
	api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
		dest := args.Get(0).(*map[string]any)
		*dest = map[string]any{"MaxEdgeLength": float64(1024)}
	}).Return(nil)
	api.On("LogWarn", mock.Anything, mock.Anything).Maybe().Return()
	api.On("LogInfo", mock.Anything, mock.Anything).Maybe().Return()
	api.On("LogDebug", mock.Anything, mock.Anything).Maybe().Return()
	// On a machine without ffmpeg — CI runners have none — the hook warns
	// that video and voice are unavailable. That warning is correct
	// behaviour, and a mock that forbids it fails the test for the
	// environment rather than for the code.
	api.On("LogWarn", mock.Anything, mock.Anything).Maybe().Return()

	p := &MediaForgePlugin{}
	p.SetAPI(api)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				cfg := p.getConfiguration()
				if cfg.MaxEdgeLength != 2560 && cfg.MaxEdgeLength != 1024 {
					t.Errorf("read a torn configuration: %+v", cfg)
					return
				}
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if err := p.OnConfigurationChange(); err != nil {
					t.Errorf("OnConfigurationChange returned an error: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestUnconfiguredPluginUsesDefaults covers the window before the server has
// called OnConfigurationChange at all — hooks must never see a zero-valued
// configuration, which would read as "compression off, edge length 0".
func TestUnconfiguredPluginUsesDefaults(t *testing.T) {
	p := &MediaForgePlugin{}
	if got := p.getConfiguration(); got != defaultConfiguration() {
		t.Errorf("configuration before OnConfigurationChange = %+v, want %+v",
			got, defaultConfiguration())
	}
}

// ---------------------------------------------------------------------------
// The manifest is the System Console surface
// ---------------------------------------------------------------------------

// TestManifestExposesEveryConfigurableSetting checks the schema an
// administrator actually sees. The manifest is what renders the page, so a
// setting that exists in Go and not here is unreachable, and a default that
// disagrees with the code is a trap: the console would show one value while the
// plugin used another.
func TestManifestExposesEveryConfigurableSetting(t *testing.T) {
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatalf("reading the manifest failed: %v", err)
	}
	var manifest model.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the manifest is not valid JSON: %v", err)
	}
	if err := manifest.IsValid(); err != nil {
		t.Fatalf("the manifest is not valid: %v", err)
	}
	if manifest.SettingsSchema == nil {
		t.Fatal("the manifest declares no settings schema, so nothing is configurable")
	}

	def := defaultConfiguration()
	byKey := map[string]*model.PluginSetting{}
	// Display-only settings are counted separately. A "custom" entry has no
	// value, no default and nothing to save: it is a panel the webapp renders,
	// and holding it to the contract below ("declares a default matching the
	// Go code") would be asserting something that cannot be true of it.
	// TestTheManifestDeclaresTheHealthPanel covers it instead.
	display := map[string]*model.PluginSetting{}
	for _, s := range allManifestSettings(manifest.SettingsSchema) {
		if s.Type == "custom" {
			display[s.Key] = s
			continue
		}
		byKey[s.Key] = s
	}

	want := []struct {
		key         string
		settingType string
		def         any
	}{
		{"EnableImageCompression", "bool", def.EnableImageCompression},
		{"MaxEdgeLength", "number", float64(def.MaxEdgeLength)},
		{"JpegQuality", "number", float64(def.JpegQuality)},
		{"MinSizeToProcessKB", "number", float64(def.MinSizeToProcessKB)},
		{"ImageMetadata", "radio", metadataPreserve},
		{"EnableVideoCompression", "bool", def.EnableVideoCompression},
		{"MaxVideoHeight", "number", float64(def.MaxVideoHeight)},
		{"VideoCodec", "radio", videoCodecH264},
		{"VideoCRF", "number", float64(def.VideoCRF)},
		{"AudioBitrateKbps", "number", float64(def.AudioBitrateKbps)},
		{"VideoTimeoutSeconds", "number", float64(def.VideoTimeoutSeconds)},
		{"MaxVideoSizeMB", "number", float64(def.MaxVideoSizeMB)},
		{"MaxConcurrentVideoJobs", "number", float64(def.MaxConcurrentVideoJobs)},
		{"MaxConcurrentImageJobs", "number", float64(def.MaxConcurrentImageJobs)},
		{"SizeNoteStyle", "radio", noteStyleFooter},
		// Read from def rather than written as `false`, so this entry also
		// pins that the console shows the value the plugin actually uses.
		// Off by default on purpose: a filename is a permanent disclosure.
		{"MarkCompressedFilenames", "bool", def.MarkCompressedFilenames},
		{"EnableVoiceMessages", "bool", def.EnableVoiceMessages},
		{"MaxVoiceSeconds", "number", float64(def.MaxVoiceSeconds)},
		// Read from def, like MarkCompressedFilenames above and for a
		// stronger version of the same reason: this is the only setting in
		// the plugin that is off by default, because it removes part of what
		// somebody said rather than re-encoding it. If the manifest and the
		// code ever disagree about that, an administrator would be switching
		// on something they believe is already on, or the reverse.
		{"TrimVoiceSilence", "bool", def.TrimVoiceSilence},
		{"VoiceChannels", "text", def.VoiceChannels},
	}

	if len(byKey) != len(want) {
		t.Errorf("the manifest declares %d settings, want %d: %v",
			len(byKey), len(want), slicesKeys(byKey))
	}

	for _, w := range want {
		t.Run(w.key, func(t *testing.T) {
			s, ok := byKey[w.key]
			if !ok {
				t.Fatalf("the manifest has no setting %q", w.key)
			}
			if s.Type != w.settingType {
				t.Errorf("type = %q, want %q", s.Type, w.settingType)
			}
			if s.Default != w.def {
				t.Errorf("default = %#v, want %#v — the console would show a value "+
					"the plugin does not use", s.Default, w.def)
			}
			if strings.TrimSpace(s.DisplayName) == "" {
				t.Error("the setting has no display name")
			}
			if len(strings.TrimSpace(s.HelpText)) < 40 {
				t.Errorf("help text is %q; administrators need the practical effect "+
					"spelled out, not a restatement of the name", s.HelpText)
			}
		})
	}

	// The health panel is the first thing on the page on purpose: it is what
	// explains an installation where video compression is silently off. A
	// "custom" entry is inert without the webapp component registered under the
	// same key, so both halves are pinned — the manifest here, the registration
	// in webapp/tests/health.test.js.
	t.Run("HealthReport", func(t *testing.T) {
		s, ok := display["HealthReport"]
		if !ok {
			t.Fatal("the manifest declares no HealthReport panel, so a missing " +
				"ffmpeg would be invisible in the System Console again")
		}
		if s.Type != "custom" {
			t.Errorf("type = %q, want \"custom\"; only a custom setting can render "+
				"a value measured on the server", s.Type)
		}
		if strings.TrimSpace(s.DisplayName) == "" {
			t.Error("the panel has no display name")
		}
		if len(strings.TrimSpace(s.HelpText)) < 40 {
			t.Errorf("help text is %q; it has to say what the panel reports and "+
				"what happens when a requirement is missing", s.HelpText)
		}
		// First in reading order, which now means the first setting of the
		// first section rather than the first of a flat list.
		ordered := allManifestSettings(manifest.SettingsSchema)
		if len(ordered) == 0 || ordered[0].Key != "HealthReport" {
			t.Errorf("the health panel is setting %d, not the first; an "+
				"administrator should meet it before the options it explains",
				indexOfSetting(ordered, "HealthReport")+1)
		}
	})

	// The radio options must be exactly the two values the code understands,
	// otherwise the console can produce a value that always falls back.
	metadata := byKey["ImageMetadata"]
	if metadata == nil {
		t.Fatal("no ImageMetadata setting")
	}
	gotOptions := map[string]bool{}
	for _, o := range metadata.Options {
		gotOptions[o.Value] = true
		if strings.TrimSpace(o.DisplayName) == "" {
			t.Errorf("option %q has no display name", o.Value)
		}
	}
	for _, v := range []string{metadataPreserve, metadataStrip} {
		if !gotOptions[v] {
			t.Errorf("the metadata setting offers no %q option; the code path is unreachable", v)
		}
	}
	if len(gotOptions) != 2 {
		t.Errorf("the metadata setting offers %d options, want exactly 2", len(gotOptions))
	}
}

// TestConcurrencyHelpTextsAreWrittenForAdministrators.
//
// The constraint on this slice is explicit: these texts are read by someone
// running a server, not by someone reading the source. They have to say what a
// higher value does to the SPEED of the machine and to the people WAITING for
// an upload — and they must say what happens when the limit is reached, because
// "your file was stored but not compressed" is exactly the kind of silent
// behaviour an administrator will otherwise discover by accident.
func TestConcurrencyHelpTextsAreWrittenForAdministrators(t *testing.T) {
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatalf("reading the manifest failed: %v", err)
	}
	var manifest model.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the manifest is not valid JSON: %v", err)
	}

	help := map[string]string{}
	for _, s := range allManifestSettings(manifest.SettingsSchema) {
		help[s.Key] = strings.ToLower(s.HelpText)
	}

	cases := []struct {
		key   string
		words []string
	}{
		// "core"/"processor" for the machine, "slow" for the effect on
		// everyone else, "stored" for what happens at the limit.
		{"MaxConcurrentVideoJobs", []string{"processor", "slow", "stored", "recommended"}},
		{"MaxConcurrentImageJobs", []string{"memory", "stored", "recommended"}},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			text := help[tc.key]
			if text == "" {
				t.Fatalf("the manifest has no help text for %s", tc.key)
			}
			for _, w := range tc.words {
				if !strings.Contains(text, w) {
					t.Errorf("the help text never mentions %q, so an administrator cannot "+
						"tell what changing this value does:\n%s", w, text)
				}
			}
			// Developer vocabulary has no place on this page.
			for _, jargon := range []string{"goroutine", "semaphore", "mutex", "thread pool", "queue depth"} {
				if strings.Contains(text, jargon) {
					t.Errorf("the help text contains developer jargon %q", jargon)
				}
			}
		})
	}
}

// TestMetadataHelpTextNamesTheLocationRisk is a requirement in its own right.
//
// PLAN the configuration surface and SPEC §4.1 both insist on it: the choice between keeping and
// removing EXIF is a privacy decision, and most administrators do not know that
// "metadata" routinely includes the GPS coordinates a photo was taken at. If
// that is not visible from the text alone, the setting is misleading however
// correct the code is.
func TestMetadataHelpTextNamesTheLocationRisk(t *testing.T) {
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

	var help string
	for _, s := range allManifestSettings(manifest.SettingsSchema) {
		if s.Key == "ImageMetadata" {
			help = strings.ToLower(s.HelpText)
		}
	}
	if help == "" {
		t.Fatal("the metadata setting has no help text")
	}

	for _, want := range []string{"location", "privacy"} {
		if !strings.Contains(help, want) {
			t.Errorf("the metadata help text never mentions %q:\n%s", want, help)
		}
	}
}

func slicesKeys(m map[string]*model.PluginSetting) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// indexOfSetting reports where a key sits in the schema, or -1.
func indexOfSetting(settings []*model.PluginSetting, key string) int {
	for i, s := range settings {
		if s.Key == key {
			return i
		}
	}
	return -1
}

// loadManifest reads and validates the manifest from disk.
func loadManifest(t *testing.T) model.Manifest {
	t.Helper()
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatalf("reading the manifest failed: %v", err)
	}
	var manifest model.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the manifest is not valid JSON: %v", err)
	}
	if err := manifest.IsValid(); err != nil {
		t.Fatalf("the manifest is not valid: %v", err)
	}
	if manifest.SettingsSchema == nil {
		t.Fatal("the manifest declares no settings schema")
	}
	return manifest
}

// allManifestSettings collects the settings from both places the schema can
// hold them.
//
// The schema has two shapes: a flat `settings` list, and `sections`, each with
// its own list. This plugin uses sections so that compression and voice
// messages are visibly separate topics rather than one undifferentiated run of
// twenty-one options.
//
// Every guard below reads through here, and that is the point: a setting moved
// into a section must not slip out of the checks that make sure it exists, has
// a default matching the Go code, and is documented. A guard that silently
// stops looking is worse than no guard, because it still reports success.
func allManifestSettings(schema *model.PluginSettingsSchema) []*model.PluginSetting {
	if schema == nil {
		return nil
	}
	out := append([]*model.PluginSetting{}, schema.Settings...)
	for _, section := range schema.Sections {
		out = append(out, section.Settings...)
	}
	return out
}

// TestTheConsoleIsGroupedIntoSections pins the grouping itself.
//
// an operator asked for it after using the console: twenty-one settings in one flat
// list gives no clue which belong together, and compression and voice messages
// are different subjects that happened to share a page.
//
// This asserts the structure rather than the wording — titles can be reworded
// without breaking a test, but a section vanishing, or a setting ending up in
// the wrong group, is a regression somebody would only notice by opening the
// console and looking.
func TestTheConsoleIsGroupedIntoSections(t *testing.T) {
	manifest := loadManifest(t)
	schema := manifest.SettingsSchema

	if len(schema.Sections) == 0 {
		t.Fatal("the console is one flat list; settings that belong together " +
			"give the reader no sign that they do")
	}

	// Where each setting is expected to live. Sections are keyed, and the key
	// is what the webapp matches on, so it is the stable thing to assert.
	want := map[string]string{
		"HealthReport":            "health",
		"EnableImageCompression":  "images",
		"MaxEdgeLength":           "images",
		"JpegQuality":             "images",
		"MinSizeToProcessKB":      "images",
		"ImageMetadata":           "images",
		"MaxConcurrentImageJobs":  "images",
		"EnableVideoCompression":  "video",
		"MaxVideoHeight":          "video",
		"VideoCodec":              "video",
		"VideoCRF":                "video",
		"AudioBitrateKbps":        "video",
		"VideoTimeoutSeconds":     "video",
		"MaxVideoSizeMB":          "video",
		"MaxConcurrentVideoJobs":  "video",
		"SizeNoteStyle":           "feedback",
		"MarkCompressedFilenames": "feedback",
		"EnableVoiceMessages":     "voice",
		"MaxVoiceSeconds":         "voice",
		"TrimVoiceSilence":        "voice",
		"VoiceChannels":           "voice",
	}

	got := map[string]string{}
	for _, section := range schema.Sections {
		if strings.TrimSpace(section.Key) == "" {
			t.Error("a section has no key; the webapp matches on the key")
		}
		if strings.TrimSpace(section.Title) == "" {
			t.Errorf("section %q has no title, so it groups without saying what "+
				"the group is", section.Key)
		}
		for _, s := range section.Settings {
			if other, clash := got[s.Key]; clash {
				t.Errorf("setting %q appears in both %q and %q", s.Key, other, section.Key)
			}
			got[s.Key] = section.Key
		}
	}

	// Nothing may be left in the flat list: a setting there renders above the
	// sections with no heading at all, which is precisely the confusion this
	// grouping removes.
	for _, s := range schema.Settings {
		t.Errorf("setting %q is outside every section and renders ungrouped", s.Key)
	}

	for key, section := range want {
		switch actual, present := got[key]; {
		case !present:
			t.Errorf("setting %q is in no section at all, so it does not render", key)
		case actual != section:
			t.Errorf("setting %q is grouped under %q; it belongs under %q",
				key, actual, section)
		}
	}
	for key := range got {
		if _, expected := want[key]; !expected {
			t.Errorf("setting %q is grouped but this test does not know it; "+
				"add it to the map so its placement is pinned too", key)
		}
	}

	// The health panel opens the console. An administrator should meet what
	// the plugin reports about itself before the options that change it.
	if schema.Sections[0].Key != "health" {
		t.Errorf("the first section is %q, not the health panel", schema.Sections[0].Key)
	}
}

// TestHelpTextIsShortEnoughToRead is the other half of what an operator asked for.
//
// The help texts had grown to essays — one was 1380 characters, and the whole
// console carried 12.6k. Text that long is not read, which makes it worse than
// shorter text that is: the reader skips it and then does not know what the
// setting does.
//
// The limit is generous on purpose. This is not a style rule; it is a ceiling
// that catches an explanation growing back into a paragraph nobody finishes.
// Where a setting genuinely needs more room — a format choice with real
// compatibility consequences — the README carries the detail.
func TestHelpTextIsShortEnoughToRead(t *testing.T) {
	const ceiling = 320

	// One documented exception, named rather than waved through by raising the
	// ceiling for everyone.
	//
	// VideoCodec offers three formats, and the thing an administrator needs is
	// which devices REFUSE each one — a video stored in a format somebody's
	// phone cannot play is not recoverable without uploading it again. That is
	// three sets of named devices, and no shorter phrasing carries it.
	// "Widely supported" would fit the ceiling and tell nobody anything.
	exempt := map[string]int{"VideoCodec": 600}

	// Markdown image references are not prose and are not read as text: the
	// note-style setting shows five previews of what the note looks like,
	// which is the one thing words cannot do here. Measuring them as help
	// text would push a setting over the ceiling for being illustrated.
	images := regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)

	manifest := loadManifest(t)
	total := 0
	for _, s := range allManifestSettings(manifest.SettingsSchema) {
		text := strings.TrimSpace(s.HelpText)
		prose := strings.TrimSpace(images.ReplaceAllString(text, ""))
		total += len(prose)

		// Something has to be said. An empty help text leaves the reader with
		// a label and a number box.
		if len(prose) < 20 && s.Type != "custom" {
			t.Errorf("%q has help text %q, which does not say what it does",
				s.Key, text)
		}
		limit := ceiling
		if special, ok := exempt[s.Key]; ok {
			limit = special
		}
		if len(prose) > limit {
			t.Errorf("%q has %d characters of help text (ceiling %d); "+
				"trim it or move the detail to the README",
				s.Key, len(prose), limit)
		}
	}

	// And the page as a whole, because twenty-one settings each just under the
	// ceiling would still be a wall of text.
	if total > 4000 {
		t.Errorf("the console carries %d characters of help text in total; "+
			"it was meant to be skimmable", total)
	}
}
