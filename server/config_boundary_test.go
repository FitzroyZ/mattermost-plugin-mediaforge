package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"testing"
)

// TestThresholdBoundariesAroundTheProbeBuffer walks the interaction between the
// configurable size threshold and the fixed header probe buffer.
//
// The probe is 512 KB + 1 byte and no longer has anything to do with the
// threshold, so the two can now sit in any relative order. This test pins every
// arrangement of them, because a mis-classification here is silent: the file is
// stored either way, just wrongly compressed or wrongly left alone.
func TestThresholdBoundariesAroundTheProbeBuffer(t *testing.T) {
	// A JPEG comfortably larger than the probe buffer.
	bigJPEG := encodeJPEG(t, noisyImage(3000, 2000, 21), 95)
	if len(bigJPEG) <= headerProbeBytes {
		t.Fatalf("the big fixture is %d bytes, not larger than the %d byte probe",
			len(bigJPEG), headerProbeBytes)
	}
	// A JPEG comfortably smaller than the probe buffer.
	smallJPEG := encodeJPEG(t, noisyImage(500, 400, 22), 90)
	if len(smallJPEG) >= headerProbeBytes {
		t.Fatalf("the small fixture is %d bytes, not smaller than the %d byte probe",
			len(smallJPEG), headerProbeBytes)
	}

	cases := []struct {
		name          string
		input         []byte
		thresholdKB   int
		wantReEncoded bool
	}{
		{
			name:          "file larger than the probe, threshold below it",
			input:         bigJPEG,
			thresholdKB:   64,
			wantReEncoded: true,
		},
		{
			name: "file larger than the probe, threshold above the file",
			// The file fills the probe, so the size is only known after
			// buffering. The late check is the one that must catch this.
			input:         bigJPEG,
			thresholdKB:   64 * 1024,
			wantReEncoded: false,
		},
		{
			name:          "file smaller than the probe, threshold below the file",
			input:         smallJPEG,
			thresholdKB:   1,
			wantReEncoded: true,
		},
		{
			name:          "file smaller than the probe, threshold above the file",
			input:         smallJPEG,
			thresholdKB:   512,
			wantReEncoded: false,
		},
		{
			name:          "threshold of zero compresses everything",
			input:         smallJPEG,
			thresholdKB:   0,
			wantReEncoded: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfiguration()
			cfg.MinSizeToProcessKB = tc.thresholdKB

			stored, outcome := runWith(t, cfg, tc.input)

			if outcome.reEncoded != tc.wantReEncoded {
				t.Errorf("reEncoded = %v, want %v (file %d bytes, threshold %d bytes, probe %d bytes)",
					outcome.reEncoded, tc.wantReEncoded,
					len(tc.input), cfg.thresholdBytes(), headerProbeBytes)
			}
			if !tc.wantReEncoded && !bytes.Equal(stored, tc.input) {
				t.Errorf("a file that should have passed through was modified: %d in, %d out",
					len(tc.input), len(stored))
			}
			if tc.wantReEncoded {
				if _, _, format := decodeBounds(t, stored); format != "jpeg" {
					t.Errorf("stored format = %q, want jpeg", format)
				}
			}
		})
	}
}

// TestThresholdIsExclusiveAtTheExactBoundary pins the comparison itself.
//
// "Only compress files above 512 KB" has to mean exactly that: a file of
// precisely the threshold size is NOT above it and must pass through. An
// off-by-one here would be invisible in normal use and would quietly
// contradict the System Console text.
func TestThresholdIsExclusiveAtTheExactBoundary(t *testing.T) {
	// Build a JPEG and pad the configured threshold to land exactly on its
	// size, so the boundary is hit to the byte.
	img := encodeJPEG(t, noisyImage(1200, 900, 23), 92)
	if len(img)%1024 != 0 {
		// Pad with trailing bytes after EOI, which decoders ignore, until the
		// length is a whole number of KB and can be expressed as a threshold.
		img = append(img, make([]byte, 1024-len(img)%1024)...)
	}
	exactKB := len(img) / 1024

	t.Run("exactly at the threshold passes through", func(t *testing.T) {
		cfg := defaultConfiguration()
		cfg.MinSizeToProcessKB = exactKB
		stored, outcome := runWith(t, cfg, img)
		if outcome.reEncoded {
			t.Errorf("a %d byte file was compressed at a %d byte threshold; "+
				"the threshold must be exclusive", len(img), cfg.thresholdBytes())
		}
		if !bytes.Equal(stored, img) {
			t.Error("the file was modified although it should have passed through")
		}
	})

	t.Run("one KB below the threshold compresses", func(t *testing.T) {
		cfg := defaultConfiguration()
		cfg.MinSizeToProcessKB = exactKB - 1
		if _, outcome := runWith(t, cfg, img); !outcome.reEncoded {
			t.Errorf("a %d byte file was not compressed at a %d byte threshold",
				len(img), cfg.thresholdBytes())
		}
	})
}

// TestHugeFakeImageIsStillNotBufferedWithAZeroThreshold re-checks the memory
// guard under the most aggressive setting an administrator can choose.
//
// With MinSizeToProcessKB = 0 every file is a candidate, so the decode budget
// is the only thing left between the plugin and a crafted 64 MB payload
// carrying PNG magic bytes. The image compression guard must still run before the file is
// buffered, on the configured path as much as on the default one.
func TestHugeFakeImageIsStillNotBufferedWithAZeroThreshold(t *testing.T) {
	// A PNG header declaring a geometry far beyond the decode budget,
	// followed by bulk. Built the same way as the image compression regression test.
	ihdr := []byte{'I', 'H', 'D', 'R',
		0x00, 0x00, 0x75, 0x30, // 30000
		0x00, 0x00, 0x75, 0x30, // 30000
		0x08, 0x06, 0x00, 0x00, 0x00,
	}
	header := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	header = binary.BigEndian.AppendUint32(header, uint32(len(ihdr)-4))
	header = append(header, ihdr...)
	header = binary.BigEndian.AppendUint32(header, crc32.ChecksumIEEE(ihdr))

	const bodySize = 64 * 1024 * 1024

	probe := &readAheadProbe{
		body:     &zeroReader{remaining: bodySize},
		maxAhead: 8 * 1024 * 1024,
	}
	src := io.MultiReader(bytes.NewReader(header), probe)
	sink := &writeTrackingWriter{probe: probe}

	cfg := defaultConfiguration()
	cfg.MinSizeToProcessKB = 0

	outcome, err := processUpload(sink, src, cfg)
	if err != nil {
		t.Fatalf("processUpload errored: %v", err)
	}
	want := int64(len(header) + bodySize)
	if outcome.bytesWritten != want {
		t.Errorf("bytesWritten = %d, want %d", outcome.bytesWritten, want)
	}
	if outcome.reEncoded {
		t.Error("an image beyond the decode budget was re-encoded")
	}
}
