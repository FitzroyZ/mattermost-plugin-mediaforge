package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// This file is frozen history, and it exists for exactly one reason: to be able
// to REMOVE a note that an older version of this plugin wrote.
//
// Until the documentation pass the size note was written in German. `stripNoteLines` takes an
// old note off a post by matching the renderings the plugin itself produces —
// which is what keeps it from deleting a user's own line that happens to
// contain an arrow, rather than matching anything arrow-shaped. Translating the
// renderings therefore made every note written before the upgrade unmatchable,
// and an edit would have left the German note in place and appended an English
// one beside it. Two notes on one compressed file is exactly what the SPEC §4.4
// contract forbids.
//
// Nothing here is ever WRITTEN. These functions only produce the strings the
// stripper looks for, and they must never be "improved", reformatted or
// translated: the moment they stop matching what the old build actually wrote,
// the note they exist to remove becomes permanent. `note_legacy_test.go` pins
// them against the wording the German build was measured to produce, so the
// test is the record: change the strings and it fails.
//
// They can be deleted once no server can still hold a post from before the
// upgrade — which in practice means never, since posts do not expire. The cost
// is two small functions that no longer change.

// legacyGermanNote renders the note the German build wrote into the message.
//
// Only the two styles that wrote into the message are covered. The block and
// footer styles lived in a message attachment, which `withoutOurAttachments`
// removes by its `Fallback` marker — a value the documentation pass did not change, so those
// two are already handled and need nothing here.
func legacyGermanNote(style string, records []noteRecord) string {
	var lines []string
	for _, r := range records {
		switch style {
		case noteStyleLine:
			lines = append(lines, fmt.Sprintf("*MediaForge: %s → %s (%d %% gespart)*",
				formatFileSizeGerman(r.Original), formatFileSizeGerman(r.Stored),
				savedPercent(r.Original, r.Stored)))
		case noteStyleCompact:
			lines = append(lines, fmt.Sprintf("`%s → %s`",
				formatFileSizeGerman(r.Original), formatFileSizeGerman(r.Stored)))
		}
	}
	return strings.Join(lines, "\n")
}

// formatFileSizeGerman writes a byte count the way the German build wrote it:
// the same decimal units, but with a comma for the decimal separator.
//
// It is a copy of `formatFileSize` as it stood before the documentation pass rather than a
// call into it with a different separator, and deliberately so. The current
// formatter is allowed to change — rounding, a unit boundary, a new unit — and
// every such change would silently alter the strings this file is supposed to
// match. A frozen copy cannot drift.
func formatFileSizeGerman(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	if bytes < 1000 {
		return fmt.Sprintf("%d B", bytes)
	}

	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(bytes)
	for i, unit := range units {
		value /= 1000
		if value >= 1000 && i < len(units)-1 {
			continue
		}

		if unit == "KB" {
			rounded := math.Round(value)
			if rounded >= 1000 {
				continue
			}
			return fmt.Sprintf("%d KB", int64(rounded))
		}

		rounded := math.Round(value*10) / 10
		if rounded >= 1000 && i < len(units)-1 {
			continue
		}
		return strings.Replace(strconv.FormatFloat(rounded, 'f', 1, 64), ".", ",", 1) + " " + unit
	}
	return fmt.Sprintf("%d B", bytes)
}
