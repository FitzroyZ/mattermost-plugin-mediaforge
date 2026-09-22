// Copyright (c) 2026 MediaForge contributors.
// See LICENSE for license information.

package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Naming for files the plugin has compressed (the file-naming work.2).
//
// Two separate problems arrived together and want different answers.
//
// FIRST: files downloaded from Mattermost had meaningless names —
//
//	rn_image_picker_lib_temp_ed17e404-bb31-4078-a596-96bbad2efa01
//
// That is NOT this plugin's doing. `rn_image_picker_lib_temp_<uuid>` is the
// temporary name React Native's image picker gives a photo, so it arrives that
// way from the Mattermost MOBILE app, long before the server sees it. Every such
// upload has looked like this since before MediaForge existed. Verified: the
// hook returns a nil FileInfo for images and videos, meaning "accept unchanged",
// and only voice recordings were ever renamed.
//
// SECOND: some operators want a visible marker so that someone downloading a file can
// see it was compressed. Decided, with the shape left open — his suggestion was
// `<original>_mm-compressed.<ext>`.
//
// The two combine into one rule: PRESERVE a real name and decorate it, INVENT a
// readable one when there is nothing worth preserving.
//
// Deliberately NOT touched: FileInfo.Path. the security audit found that rewriting it
// orphans the already written file — the bytes are on disk under the old path.
// Renaming here is a display concern only.

// compressedSuffix is appended to a name whose file the plugin has shrunk.
const compressedSuffix = "_mm-compressed"

// placeholderNamePatterns match names carrying no information about the file.
//
// Each entry is a shape observed in practice rather than a guess, because a
// false positive here THROWS AWAY a name a person chose. When a pattern does not
// match, the name is kept — the failure mode is "keeps an ugly name", never
// "destroys a good one".
var placeholderNamePatterns = []*regexp.Regexp{
	// React Native image picker, Mattermost mobile. reported real-world examples.
	regexp.MustCompile(`(?i)^rn_image_picker_lib_temp_[0-9a-f-]{8,}$`),
	// Android/iOS camera roll exports that are only a timestamp in ms.
	regexp.MustCompile(`^[0-9]{13,}$`),
	// A bare UUID, with or without separators.
	regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`),
	// What Mattermost itself calls a pasted image with no name.
	regexp.MustCompile(`(?i)^image pasted at `),
}

// looksLikePlaceholder reports whether a stem carries no useful information.
func looksLikePlaceholder(stem string) bool {
	trimmed := strings.TrimSpace(stem)
	if trimmed == "" {
		return true
	}
	for _, pattern := range placeholderNamePatterns {
		if pattern.MatchString(trimmed) {
			return true
		}
	}
	return false
}

// kindPrefix names the generated file after what it is, so a download folder
// full of them is still navigable.
func kindPrefix(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	default:
		return "photo"
	}
}

// compressedName returns the name a compressed file should carry.
//
// `at` is passed in rather than read from the clock so the behaviour is testable
// and two files uploaded in the same second are still distinguishable by the
// caller if it chooses to vary it.
func compressedName(original, mimeType string, at time.Time, addSuffix bool) string {
	ext := filepath.Ext(original)
	stem := strings.TrimSuffix(original, ext)

	if looksLikePlaceholder(stem) {
		// Nothing worth preserving: invent something readable. The suffix is
		// pointless here — a generated name is not a name anybody chose, so
		// there is no "original" for a reader to be told about.
		return fmt.Sprintf("%s-%s%s", kindPrefix(mimeType), at.Format("20060102-150405"), ext)
	}

	if !addSuffix {
		return original
	}
	// Idempotent: re-processing a file must not produce
	// `photo_mm-compressed_mm-compressed.jpg`.
	if strings.HasSuffix(stem, compressedSuffix) {
		return original
	}
	return stem + compressedSuffix + ext
}
