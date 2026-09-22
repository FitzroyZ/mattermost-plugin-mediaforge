// Copyright (c) 2026 MediaForge contributors.
// See LICENSE for license information.

package main

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// namedResult is what the hook returns for a compressed file, and it decides
// two things at once: the rename (every compressed file) and the geometry
// correction (only some). The reason it exists is that the branches which used
// to decide this separately each had a `return nil` path — and a rename threaded
// through those by hand is how one gets forgotten.

var namedAt = time.Date(2026, 9, 19, 13, 14, 15, 0, time.UTC)

func TestNamedResultAcceptsUnchangedWhenNothingChanged(t *testing.T) {
	// A nil result means "accept unchanged", and that must stay the answer when
	// neither the name nor the geometry moved. Returning a pointer needlessly
	// makes the server save a row it did not have to.
	info := &model.FileInfo{Name: "urlaubsfoto.jpg", MimeType: "image/jpeg"}
	cfg := configuration{MarkCompressedFilenames: false}

	if got := namedResult(info, cfg, 0, 0, false, namedAt); got != nil {
		t.Errorf("expected nil (accept unchanged), got name=%q", got.Name)
	}
}

func TestNamedResultRenamesEvenWhenGeometryDidNotChange(t *testing.T) {
	// THE regression this function exists for. The image path returned nil when
	// the dimensions were already right, which would have dropped the rename
	// for every correctly rotated photo — the common case.
	info := &model.FileInfo{
		Name:     "rn_image_picker_lib_temp_ed17e404-bb31-4078-a596-96bbad2efa01.jpg",
		MimeType: "image/jpeg",
	}
	cfg := configuration{MarkCompressedFilenames: false}

	got := namedResult(info, cfg, 0, 0, false, namedAt)
	if got == nil {
		t.Fatal("a placeholder name was left in place because the geometry was already correct")
	}
	if want := "photo-20260919-131415.jpg"; got.Name != want {
		t.Errorf("got %q, want %q", got.Name, want)
	}
}

func TestNamedResultAppliesGeometryAndNameTogether(t *testing.T) {
	info := &model.FileInfo{Name: "urlaubsfoto.jpg", MimeType: "image/jpeg", Width: 4000, Height: 3000}
	cfg := configuration{MarkCompressedFilenames: true}

	got := namedResult(info, cfg, 2560, 1920, true, namedAt)
	if got == nil {
		t.Fatal("expected a corrected FileInfo")
	}
	if want := "urlaubsfoto_mm-compressed.jpg"; got.Name != want {
		t.Errorf("name: got %q, want %q", got.Name, want)
	}
	if got.Width != 2560 || got.Height != 1920 {
		t.Errorf("geometry: got %dx%d, want 2560x1920", got.Width, got.Height)
	}
}

func TestNamedResultLeavesGeometryAloneWhenItDidNotChange(t *testing.T) {
	// A rename must not smuggle in dimensions the caller did not vouch for.
	// Passing 0x0 with geometryChanged=false is how the video path reports
	// "ffprobe told me nothing", and writing that over real dimensions would
	// make a client reserve no space for the picture.
	info := &model.FileInfo{Name: "clip.mp4", MimeType: "video/mp4", Width: 1920, Height: 1080}
	cfg := configuration{MarkCompressedFilenames: true}

	got := namedResult(info, cfg, 0, 0, false, namedAt)
	if got == nil {
		t.Fatal("expected a renamed FileInfo")
	}
	if got.Width != 1920 || got.Height != 1080 {
		t.Errorf("geometry was overwritten: got %dx%d, want 1920x1080", got.Width, got.Height)
	}
}

func TestNamedResultNeverTouchesPath(t *testing.T) {
	// the security audit found that rewriting Path orphans the file: the bytes are
	// already on disk under the old path. The rename is display only.
	info := &model.FileInfo{
		Name:     "rn_image_picker_lib_temp_ed17e404-bb31-4078-a596-96bbad2efa01.jpg",
		MimeType: "image/jpeg",
		Path:     "20260919/teams/abc/channels/def/users/ghi/jkl/original.jpg",
	}
	cfg := configuration{MarkCompressedFilenames: true}

	got := namedResult(info, cfg, 100, 100, true, namedAt)
	if got == nil {
		t.Fatal("expected a FileInfo")
	}
	if got.Path != info.Path {
		t.Errorf("Path was rewritten, which orphans the stored file:\n got %q\nwant %q",
			got.Path, info.Path)
	}
}

func TestNamedResultDoesNotMutateTheInput(t *testing.T) {
	// The hook is handed a pointer the server also holds. Writing through it
	// instead of copying would change a row nobody asked to change.
	info := &model.FileInfo{Name: "urlaubsfoto.jpg", MimeType: "image/jpeg", Width: 4000}
	cfg := configuration{MarkCompressedFilenames: true}

	_ = namedResult(info, cfg, 2560, 1920, true, namedAt)

	if info.Name != "urlaubsfoto.jpg" || info.Width != 4000 {
		t.Errorf("the input was mutated: name=%q width=%d", info.Name, info.Width)
	}
}

func TestNamedResultSurvivesANilInfo(t *testing.T) {
	if got := namedResult(nil, configuration{}, 1, 1, true, namedAt); got != nil {
		t.Error("a nil info produced a result")
	}
}

func TestMarkCompressedFilenamesIsOnByDefault(t *testing.T) {
	// Decided after trying it on a real server on his own server: someone downloading
	// a file should be able to see that it was compressed. The earlier default
	// was off, on the argument that a filename is a disclosure nobody asked
	// for — he asked for it, so the default follows the decision.
	if !defaultConfiguration().MarkCompressedFilenames {
		t.Error("the suffix is off by default; an operator asked for it to be the " +
			"default after finding he had to switch it on by hand")
	}
}

func TestMarkCompressedFilenamesCanStillBeSwitchedOff(t *testing.T) {
	// The property that matters more than the default: the disclosure travels
	// with the file and cannot be taken back later, unlike the size note which
	// SizeNoteStyle can switch off. So an operator must be able to decline it —
	// a default is a recommendation, not a lock.
	cfg := configuration{MarkCompressedFilenames: false}
	info := &model.FileInfo{Name: "urlaubsfoto.jpg", MimeType: "image/jpeg"}

	if got := namedResult(info, cfg, 0, 0, false, namedAt); got != nil {
		t.Errorf("the suffix was applied with the setting off: %q", got.Name)
	}
}
