// Copyright (c) 2026 MediaForge contributors.
// See LICENSE for license information.

package main

import (
	"testing"
	"time"
)

var namingClock = time.Date(2026, 9, 19, 13, 14, 15, 0, time.UTC)

// The property that matters most: a name a person chose is never destroyed.
//
// The failure mode of a too-eager placeholder rule is silent and permanent — a
// file called `Rechnung-2026-09.pdf` renamed to `photo-20260919-131415.pdf` has
// lost information nobody can recover. Every one of these must survive.
func TestCompressedNameKeepsNamesAPersonChose(t *testing.T) {
	keep := []string{
		"AlphaBalm-grey-closeup-2.jpg",
		"urlaubsfoto.jpg",
		"GENTLEMAN 0.28mm.jpg",
		"微信图片_20260912175656_102_36.jpg",
		"IMG_2024.jpg",
		"a.png",
		// Digits, but not the 13-digit millisecond shape.
		"2026-09-19.jpg",
		"12345.jpg",
		// Looks uuid-ish but is not one.
		"deadbeef-cafe.jpg",
	}
	for _, name := range keep {
		got := compressedName(name, "image/jpeg", namingClock, false)
		if got != name {
			t.Errorf("a chosen name was rewritten: %q became %q", name, got)
		}
	}
}

func TestCompressedNameAddsTheSuffixWhenAsked(t *testing.T) {
	got := compressedName("urlaubsfoto.jpg", "image/jpeg", namingClock, true)
	if want := "urlaubsfoto_mm-compressed.jpg"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCompressedNameLeavesTheNameAloneWhenTheSuffixIsOff(t *testing.T) {
	// The suffix is a permanent disclosure to every recipient, so it is a
	// setting, not a rule for every operator of this plugin.
	got := compressedName("urlaubsfoto.jpg", "image/jpeg", namingClock, false)
	if want := "urlaubsfoto.jpg"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCompressedNameDoesNotStackSuffixes(t *testing.T) {
	// Re-processing must not produce photo_mm-compressed_mm-compressed.jpg.
	once := compressedName("photo.jpg", "image/jpeg", namingClock, true)
	twice := compressedName(once, "image/jpeg", namingClock, true)
	if twice != once {
		t.Errorf("suffix stacked: %q became %q", once, twice)
	}
}

// The mobile case that started this: there is no original name to decorate.
func TestCompressedNameInventsAReadableNameForPlaceholders(t *testing.T) {
	cases := []struct {
		name     string
		mimeType string
		want     string
	}{
		{
			// the operator's example, verbatim.
			"rn_image_picker_lib_temp_ed17e404-bb31-4078-a596-96bbad2efa01.jpg",
			"image/jpeg",
			"photo-20260919-131415.jpg",
		},
		{
			"rn_image_picker_lib_temp_b4287d68-506b-4e51-9443-d4e61d34bc58.mp4",
			"video/mp4",
			"video-20260919-131415.mp4",
		},
		{
			// Camera roll export: milliseconds since the epoch.
			"1789455381460.jpg", "image/jpeg", "photo-20260919-131415.jpg",
		},
		{
			// A bare UUID.
			"ed17e404-bb31-4078-a596-96bbad2efa01.png",
			"image/png",
			"photo-20260919-131415.png",
		},
		{
			// What Mattermost names a nameless paste.
			"Image Pasted at 2026-9-19 13-14.png",
			"image/png",
			"photo-20260919-131415.png",
		},
	}
	for _, tc := range cases {
		got := compressedName(tc.name, tc.mimeType, namingClock, false)
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGeneratedNamesCarryNoSuffixEvenWhenEnabled(t *testing.T) {
	// A generated name is not a name anybody chose, so there is no "original"
	// a reader could be told about. Appending the marker would only make an
	// already synthetic name longer.
	got := compressedName(
		"rn_image_picker_lib_temp_ed17e404-bb31-4078-a596-96bbad2efa01.jpg",
		"image/jpeg", namingClock, true)
	if want := "photo-20260919-131415.jpg"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCompressedNameKeepsTheExtension(t *testing.T) {
	// The extension decides whether a downloaded file opens at all.
	for _, tc := range []struct{ in, want string }{
		{"rn_image_picker_lib_temp_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jpeg", ".jpeg"},
		{"1789455381460.HEIC", ".HEIC"},
	} {
		got := compressedName(tc.in, "image/jpeg", namingClock, false)
		if ext := got[len(got)-len(tc.want):]; ext != tc.want {
			t.Errorf("%q: extension became %q, want %q", tc.in, ext, tc.want)
		}
	}
}

func TestCompressedNameHandlesAnExtensionlessPlaceholder(t *testing.T) {
	// the reported examples had NO extension at all — the names he read off his
	// downloads folder end at the uuid.
	got := compressedName(
		"rn_image_picker_lib_temp_ed17e404-bb31-4078-a596-96bbad2efa01",
		"image/jpeg", namingClock, false)
	if want := "photo-20260919-131415"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCompressedNameHandlesAnEmptyName(t *testing.T) {
	got := compressedName("", "image/jpeg", namingClock, false)
	if want := "photo-20260919-131415"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
