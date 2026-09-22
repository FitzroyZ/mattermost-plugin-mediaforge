package main

import (
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// This file covers the one way the the documentation pass translation is NOT merely a text
// change: a post that already carries a note written by the German build.
//
// `stripNoteLines` removes an old note by matching the renderings the plugin
// itself would produce, which is what keeps it from deleting a user's own line
// that happens to contain an arrow. Translating those renderings therefore
// makes every note written before the upgrade unmatchable — and an edit would
// append a second, English note beside the German one still sitting in the
// message. Two notes on one file is exactly what the SPEC §4.4 contract
// forbids, and it would appear only when somebody edits an old post, which is
// the kind of thing nobody reproduces by hand.

// TestAnEditStripsTheGermanNoteTheOldBuildWrote is the direct reproduction.
func TestAnEditStripsTheGermanNoteTheOldBuildWrote(t *testing.T) {
	records := []noteRecord{{Name: "big.jpg", Original: 11_906_359, Stored: 2_323_820}}

	// Exactly what the German build wrote, from the the size note server proof.
	for _, was := range []string{
		"*MediaForge: 11,9 MB → 2,3 MB (80 % gespart)*",
		"`11,9 MB → 2,3 MB`",
	} {
		message := "Have a look at this!\n" + was

		stripped := stripNoteLines(message, records)
		if strings.Contains(stripped, was) {
			t.Errorf("the German note the old build wrote survived stripping, so an edit "+
				"would leave the post carrying two notes:\n  %q", stripped)
		}
		if !strings.Contains(stripped, "Have a look at this!") {
			t.Errorf("stripping took the user's own words with it: %q", stripped)
		}
	}
}

// TestAPostFromTheGermanBuildEndsUpWithExactlyOneNoteAfterAnEdit walks the whole
// path rather than the helper, because the helper is not what the contract is
// about.
func TestAPostFromTheGermanBuildEndsUpWithExactlyOneNoteAfterAnEdit(t *testing.T) {
	deadline(t, 60*time.Second)

	np := newNotePlugin(t, map[string]any{"SizeNoteStyle": noteStyleLine})

	// A post as the German build would have stored it: the old note in the
	// message, and the bookkeeping prop that carries the two sizes.
	old := &model.Post{
		Message: "Have a look at this!\n*MediaForge: 11,9 MB → 2,3 MB (80 % gespart)*",
	}
	old.AddProp(notePropKey, encodeRecords([]noteRecord{
		{Name: "big.jpg", Original: 11_906_359, Stored: 2_323_820},
	}))

	edited := old.Clone()
	edited.Message = "Have a look at this! (edited)\n*MediaForge: 11,9 MB → 2,3 MB (80 % gespart)*"

	updated, reason := np.MessageWillBeUpdated(&plugin.Context{}, edited, old)
	if reason != "" {
		t.Fatalf("the edit was refused: %s", reason)
	}
	if updated == nil {
		t.Fatal("the edit returned no post, so the note was not re-rendered at all")
	}

	notes := 0
	for _, line := range strings.Split(updated.Message, "\n") {
		if strings.Contains(line, "MediaForge") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("the edited post carries %d note(s), want exactly 1 — SPEC §4.4 allows one "+
			"note per compressed file and no more:\n%s", notes, updated.Message)
	}
	if !strings.Contains(updated.Message, "saved)") {
		t.Errorf("the note was not re-rendered in the current English style: %q", updated.Message)
	}
	if strings.Contains(updated.Message, "gespart") {
		t.Errorf("the German note is still there: %q", updated.Message)
	}
	if !strings.HasPrefix(updated.Message, "Have a look at this! (edited)") {
		t.Errorf("the user's own edit was lost: %q", updated.Message)
	}
}

// TestTheLegacyRenderingsAreTheOnesTheGermanBuildActuallyWrote pins the strings
// the stripper now has to know about.
//
// They are frozen history: they must match what the size note shipped, which is
// what the German build was measured to write, and they must never be
// "improved". This test is that record: it is the only place the old wording
// survives, so changing the strings to look nicer breaks it on purpose.
func TestTheLegacyRenderingsAreTheOnesTheGermanBuildActuallyWrote(t *testing.T) {
	records := []noteRecord{{Name: "big.jpg", Original: 11_906_359, Stored: 2_323_820}}

	cases := map[string]string{
		noteStyleLine:    "*MediaForge: 11,9 MB → 2,3 MB (80 % gespart)*",
		noteStyleCompact: "`11,9 MB → 2,3 MB`",
	}
	for style, want := range cases {
		if got := legacyGermanNote(style, records); got != want {
			t.Errorf("the legacy %s rendering is %q, want %q — this string is history and "+
				"must match what the German build actually wrote, or the note it left on "+
				"a post can never be removed", style, got, want)
		}
	}

	// A style the German build never wrote into a message renders nothing, so
	// the stripper does not go looking for a line that cannot exist.
	for _, style := range []string{noteStyleBlock, noteStyleFooter, noteStyleNone} {
		if got := legacyGermanNote(style, records); got != "" {
			t.Errorf("the legacy renderer produced %q for %s, which never wrote into the "+
				"message at all", got, style)
		}
	}
}

// TestTheGermanSizeFormatIsFrozen guards the other half of the legacy strings.
func TestTheGermanSizeFormatIsFrozen(t *testing.T) {
	cases := map[int64]string{
		0:             "0 B",
		999:           "999 B",
		340_000:       "340 KB",
		2_323_820:     "2,3 MB",
		11_906_359:    "11,9 MB",
		1_000_000_000: "1,0 GB",
	}
	for bytes, want := range cases {
		if got := formatFileSizeGerman(bytes); got != want {
			t.Errorf("formatFileSizeGerman(%d) = %q, want %q", bytes, got, want)
		}
	}
}
