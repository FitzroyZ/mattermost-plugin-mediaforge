package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/mattermost/mattermost/server/public/model"
)

// This file holds the one invariant the documentation pass adds: everything a human reads —
// the System Console, the note on the post, the record button, the report — is
// written in English.
//
// It exists because the plugin shipped earlier work with exactly one German
// setting among eighteen English ones. That was a deliberate, recorded
// decision (the wording had been approved on a running instance and rewriting
// it mid-slice would have reopened a freshly proved feature), but it is not a
// state a public repository can stay in. Nothing in the plugin notices a
// German string: it renders, it is valid, every contract test passes. Only a
// reader notices, and a reader is exactly who is not in the loop.

// germanMarkers are words and letters that occur in German and not in English.
//
// The list is deliberately made of function words rather than of the note's own
// vocabulary. A guard built from "gespart" and "kleiner" would only catch the
// exact strings this slice removed, and would wave through the next German
// sentence somebody adds. Function words are what any German text is built
// from, so they catch a sentence nobody thought of when this was written.
//
// Every entry is checked to be absent from English as well, because a marker
// that also occurs in English would fail on a correct text and would then be
// deleted rather than trusted.
var germanMarkers = []string{
	"der", "die", "das", "und", "nicht", "mit", "ist", "sind", "für",
	"von", "auf", "eine", "einen", "einem", "kein", "keine", "keinen",
	"wird", "werden", "kann", "können", "muss", "müssen", "durch",
	"nur", "unter", "über", "wenn", "dann", "beim", "zum", "zur",
	"Datei", "Dateien", "Nachricht", "Nachrichten", "Beitrag", "Zeile",
	"Fußzeile", "Hinweis", "Ersparnis", "gespart", "kleiner", "Zahlen",
	"Variante", "empfohlen", "Balken", "abgesetzter", "farbigem",
	"Sekunden", "Bilder", "Video", "Einstellung", "Server", "Speicher",
}

// englishAllowed are words on the marker list that are also perfectly good
// English, so they are removed from the check rather than left to produce a
// false alarm on a correct text.
//
// "Server" and "Video" are the obvious ones: both are spelled identically in
// the two languages. "die" is the one that is easy to miss — it reads as a
// German article and is an ordinary English verb, so a help text explaining
// that an upload does not die when compression fails would be flagged as
// German. A guard that flags correct text gets switched off rather than fixed,
// which is worse than no guard at all.
//
// They are kept in the list above rather than deleted so a later reader can see
// they were considered and rejected, which is cheaper than rediscovering it.
var englishAllowed = map[string]bool{
	"Server": true,
	"Video":  true,
	"die":    true,
}

// germanLetters are the letters English does not have at all.
const germanLetters = "äöüßÄÖÜ"

// findGerman reports the German markers in a text, or nothing.
func findGerman(text string) []string {
	var found []string

	for _, r := range text {
		if strings.ContainsRune(germanLetters, r) {
			found = append(found, fmt.Sprintf("the letter %q", string(r)))
			break
		}
	}

	// Word boundaries matter: "der" must not fire on "under", and "ist" must
	// not fire on "exist" or on "statistics".
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r)
	})
	seen := map[string]bool{}
	for _, w := range words {
		for _, marker := range germanMarkers {
			if englishAllowed[marker] {
				continue
			}
			if strings.EqualFold(w, marker) && !seen[marker] {
				found = append(found, fmt.Sprintf("the word %q", w))
				seen[marker] = true
			}
		}
	}
	return found
}

// TestTheSystemConsoleIsEntirelyInEnglish walks every text the manifest puts
// on the administrator's screen.
//
// It reads plugin.json rather than the Go defaults on purpose: the manifest is
// what renders the page, so it is the only file whose wording an administrator
// actually sees. A German string in the Go source that never reaches the
// manifest would be a comment; a German string here is the page.
func TestTheSystemConsoleIsEntirelyInEnglish(t *testing.T) {
	raw, err := os.ReadFile("../plugin.json")
	if err != nil {
		t.Fatalf("reading the manifest failed: %v", err)
	}
	var manifest model.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the manifest is not valid JSON: %v", err)
	}
	if manifest.SettingsSchema == nil {
		t.Fatal("the manifest declares no settings schema, so there is nothing to check")
	}

	check := func(where, text string) {
		if found := findGerman(text); len(found) > 0 {
			t.Errorf("%s is not English (%s): %q", where, strings.Join(found, ", "), text)
		}
	}

	check("the schema header", manifest.SettingsSchema.Header)
	check("the schema footer", manifest.SettingsSchema.Footer)
	check("the plugin description", manifest.Description)
	check("the plugin name", manifest.Name)

	for _, s := range allManifestSettings(manifest.SettingsSchema) {
		check(fmt.Sprintf("the display name of %s", s.Key), s.DisplayName)
		check(fmt.Sprintf("the help text of %s", s.Key), s.HelpText)
		check(fmt.Sprintf("the placeholder of %s", s.Key), s.Placeholder)
		for _, o := range s.Options {
			check(fmt.Sprintf("the option %q of %s", o.Value, s.Key), o.DisplayName)
		}
	}
}

// TestEveryNoteRenderingIsInEnglish checks the four texts that land in a
// user's own post.
//
// These are the most visible strings the plugin has — they are written into
// somebody else's message — and they are the ones the note contract from
// SPEC §4.4 depends on being recognisable. Checking the rendering rather than
// the source keeps the test honest about what a reader sees.
func TestEveryNoteRenderingIsInEnglish(t *testing.T) {
	record := noteRecord{Name: "photo.jpg", Original: 11_906_359, Stored: 2_323_820}

	for _, style := range []string{noteStyleLine, noteStyleCompact} {
		text := messageNote(style, []noteRecord{record})
		if text == "" {
			t.Fatalf("the %s style rendered nothing, so the check proves nothing", style)
		}
		if found := findGerman(text); len(found) > 0 {
			t.Errorf("the %s note is not English (%s): %q", style, strings.Join(found, ", "), text)
		}
		t.Logf("%s: %s", style, text)
	}

	for _, style := range []string{noteStyleBlock, noteStyleFooter} {
		attachment := attachmentNote(style, []noteRecord{record})
		if attachment == nil {
			t.Fatalf("the %s style rendered nothing, so the check proves nothing", style)
		}
		for name, text := range map[string]string{"text": attachment.Text, "fallback": attachment.Fallback} {
			if found := findGerman(text); len(found) > 0 {
				t.Errorf("the %s of the %s note is not English (%s): %q",
					name, style, strings.Join(found, ", "), text)
			}
		}
		t.Logf("%s: %s", style, attachment.Text)
	}
}

// TestTheEnglishNoteIsNoLongerThanTheGermanOneItReplaced guards the layout.
//
// The note sits inside a post, and the footer variant sits in an attachment
// that a narrow client wraps. A translation that grew by a third would push
// the line onto two rows on a phone, which is a design change disguised as a
// text change. The German wording is pinned here as the budget it has to fit
// in, with the specific numbers from the the size note proof so the comparison is
// against something that was actually measured on a real post.
func TestTheEnglishNoteIsNoLongerThanTheGermanOneItReplaced(t *testing.T) {
	record := noteRecord{Name: "big.jpg", Original: 11_906_359, Stored: 2_323_820}

	// What the size note shipped and proved on the running instance.
	german := map[string]string{
		noteStyleLine:    "*MediaForge: 11,9 MB → 2,3 MB (80 % gespart)*",
		noteStyleCompact: "`11,9 MB → 2,3 MB`",
		noteStyleBlock:   "📦 11,9 MB → 2,3 MB · 80 % gespart durch MediaForge",
		noteStyleFooter:  "MediaForge · 11,9 MB → 2,3 MB (80 % kleiner)",
	}

	for style, was := range german {
		var now string
		switch style {
		case noteStyleLine, noteStyleCompact:
			now = messageNote(style, []noteRecord{record})
		default:
			now = attachmentNote(style, []noteRecord{record}).Text
		}

		// Counted in runes rather than bytes: "→" is three bytes and one
		// character, and it is the character count a line break follows.
		wasLen, nowLen := len([]rune(was)), len([]rune(now))
		if nowLen > wasLen {
			t.Errorf("the English %s note is %d characters against the German %d, so it may "+
				"wrap where the proved design did not:\n  was: %s\n  now: %s",
				style, nowLen, wasLen, was, now)
		}
		t.Logf("%-8s %2d → %2d characters: %s", style, wasLen, nowLen, now)
	}
}

// TestNoProductionSourceFileCarriesGermanText sweeps the files themselves.
//
// The two tests above check what is rendered, which is what matters to a
// reader — but a German comment in a repository that goes public is a reader's
// problem too, and a German string constant that is only reached on an error
// path would not be rendered by any test. This reads the source.
//
// Test files are excluded, because this very file has to name German words in
// order to look for them.
func TestNoProductionSourceFileCarriesGermanText(t *testing.T) {
	roots := []string{".", "../webapp/src", "../build"}

	checked := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "dist" || d.Name() == "testdata" || d.Name() == "public" {
					return filepath.SkipDir
				}
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".go" && ext != ".js" && ext != ".css" {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".test.js") {
				return nil
			}
			// note_legacy.go is the one deliberate exception, and it is
			// checked rather than merely skipped: it must contain German,
			// because its whole job is to reproduce the strings an older
			// build wrote so they can be removed from a post. Skipping it
			// silently would let any other German text hide there, so the
			// file is required to be what it claims to be.
			if filepath.Base(path) == "note_legacy.go" {
				raw, readErr := os.ReadFile(path)
				if readErr != nil {
					return readErr
				}
				if !strings.Contains(string(raw), "gespart") {
					t.Errorf("%s no longer contains the German note it exists to remove, so "+
						"a note written before the documentation pass can never be stripped from a post",
						path)
				}
				checked++
				return nil
			}

			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			checked++
			for i, line := range strings.Split(string(raw), "\n") {
				if found := findGerman(line); len(found) > 0 {
					t.Errorf("%s:%d is not English (%s): %s",
						path, i+1, strings.Join(found, ", "), strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s failed: %v", root, err)
		}
	}

	if checked < 10 {
		t.Fatalf("only %d source files were read, so the sweep found nothing because it "+
			"looked nowhere", checked)
	}
	t.Logf("%d production source files read", checked)
}

// TestTheGermanMarkersWouldActuallyFire is the test for the test.
//
// A guard made of words that never match is indistinguishable from a guard
// that works, and it is the more likely of the two: one typo in the list, or
// one word that turns out to be English as well, and every check above passes
// for ever. This feeds it the German texts this slice removed.
func TestTheGermanMarkersWouldActuallyFire(t *testing.T) {
	removed := []string{
		"Hinweis auf die Ersparnis",
		"1 — Zeile unter der Nachricht, kursiv",
		"2 — Nur die Zahlen, als kleines Feld",
		"3 — Abgesetzter Block mit farbigem Balken",
		"4 — Kleine Fußzeile (empfohlen)",
		"5 — Kein Hinweis",
		"MediaForge · 11,9 MB → 2,3 MB (80 % kleiner)",
		"*MediaForge: 11,9 MB → 2,3 MB (80 % gespart)*",
		"📦 11,9 MB → 2,3 MB · 80 % gespart durch MediaForge",
		"Schaut euch das mal an!",
	}
	for _, text := range removed {
		if found := findGerman(text); len(found) == 0 {
			t.Errorf("the guard does not recognise %q as German, so it would not have "+
				"stopped it coming back", text)
		}
	}

	// And it must not fire on the English the plugin actually ships. A guard
	// that flags correct text gets switched off rather than fixed.
	english := []string{
		// The words that are also English, which is the failure mode that
		// gets a guard switched off rather than fixed. "die" in particular
		// reads as a German article and is an ordinary English verb, so a
		// sentence like the first one below must not be flagged.
		"An upload does not die because compression failed.",
		"The die is cast; a video that cannot be converted is refused.",
		"Restart the server and try the video again.",
		"MediaForge · 11.9 MB → 2.3 MB (80% smaller)",
		"*MediaForge: 11.9 MB → 2.3 MB (80% saved)*",
		"📦 11.9 MB → 2.3 MB · 80% saved by MediaForge",
		"How long one spoken message may be. The recording stops by itself when it " +
			"reaches this length, and the server refuses anything longer.",
		"Size note on the post",
		"Compress this many images at the same time",
		"Only compress files above (KB)",
		"Photo information (EXIF)",
		"Files that are already small are always stored exactly as they were sent.",
		"Recommended. Plays everywhere: Windows, Mac, iPhone, Android, all browsers.",
		"About 30% smaller at equal quality, but Firefox and older Android devices " +
			"often cannot play it.",
	}
	for _, text := range english {
		if found := findGerman(text); len(found) > 0 {
			t.Errorf("the guard wrongly flags English text as German (%s): %q",
				strings.Join(found, ", "), text)
		}
	}
}

// ---------------------------------------------------------------- previews

// TestThePreviewImagesAreShippedAndUsable checks the five pictures that explain
// the SizeNoteStyle setting.
//
// They carry more of the explanation than any text does: Mattermost renders a
// radio option as plain text, so the picture is the only place a variant can
// actually be shown. What a test can check is that all five exist, are real
// PNGs of a sensible size, differ from one another, and are reachable under the
// names the manifest embeds. Whether they are UNDERSTANDABLE is a judgement a
// person makes by looking at them, and this test does not pretend otherwise.
func TestThePreviewImagesAreShippedAndUsable(t *testing.T) {
	seen := map[string]int{}

	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("note-%d.png", i)
		data, err := noteImages.ReadFile("public/" + name)
		if err != nil {
			t.Fatalf("%s is not embedded in the binary: %v", name, err)
		}

		// The PNG signature, so a truncated or mislabelled file is caught
		// here rather than by an administrator looking at a broken icon.
		if len(data) < 8 || string(data[1:4]) != "PNG" {
			t.Errorf("%s does not start with the PNG signature", name)
			continue
		}

		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Errorf("%s cannot be decoded: %v", name, err)
			continue
		}
		if format != "png" {
			t.Errorf("%s decodes as %s, but the route serves it as image/png", name, format)
		}

		// A width every variant shares, because they are shown one under
		// the other and a ragged column reads as a mistake. The height
		// differs per variant and is measured by the generator script.
		if cfg.Width != 410 {
			t.Errorf("%s is %d pixels wide, want 410 — the five are shown as a column "+
				"and must line up", name, cfg.Width)
		}
		if cfg.Height < 90 || cfg.Height > 260 {
			t.Errorf("%s is %d pixels tall, which is outside the range a preview of one "+
				"post can plausibly be", name, cfg.Height)
		}

		// Two identical images would mean one variant is illustrated by
		// another variant's picture, which is worse than no picture:
		// it actively misinforms.
		sum := fmt.Sprintf("%x", sha256.Sum256(data))
		if other, ok := seen[sum]; ok {
			t.Errorf("%s is byte-identical to note-%d.png, so one of the two variants is "+
				"illustrated by the wrong picture", name, other)
		}
		seen[sum] = i

		t.Logf("%s: %dx%d, %d bytes", name, cfg.Width, cfg.Height, len(data))
	}
}

// TestThePreviewImagesCanBeRebuilt checks that the recipe is in the repository.
//
// earlier work shipped five images nobody could reproduce: they had been made
// once, by hand, and the only record of how was in a chat log that no longer
// existed. the documentation pass had to rebuild them from scratch, which is exactly the work
// this test exists to prevent next time.
//
// It deliberately does NOT run the script: it needs a browser, it writes into
// the source tree, and a test suite that repaints the shipped images would make
// every run produce a diff. What it checks is that the script is there, is
// executable, and still names all five files — a generator that has drifted
// away from what is shipped is the same failure in slow motion.
func TestThePreviewImagesCanBeRebuilt(t *testing.T) {
	const script = "../scripts/make-note-previews.sh"

	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("the preview images have no generator script: %v — they were produced "+
			"once by hand before, and that is how the documentation pass came to rebuild all five", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("%s is not executable, so the instruction in the README does not work", script)
	}

	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("reading the generator failed: %v", err)
	}
	source := string(raw)

	for i := 1; i <= 5; i++ {
		if !strings.Contains(source, fmt.Sprintf("render %d ", i)) {
			t.Errorf("the generator does not produce variant %d, so that image cannot be "+
				"rebuilt from this repository", i)
		}
	}

	// The wording in the pictures has to be the wording in the notes,
	// otherwise the System Console shows an administrator a preview of a
	// note the plugin no longer writes.
	record := noteRecord{Original: 11_906_359, Stored: 2_323_820}
	for _, want := range []string{
		messageNote(noteStyleLine, []noteRecord{record}),
		attachmentNote(noteStyleBlock, []noteRecord{record}).Text,
		attachmentNote(noteStyleFooter, []noteRecord{record}).Text,
	} {
		// The script writes the line as HTML, so the plugin's Markdown
		// emphasis markers are not in it; the sentence inside them is.
		plain := strings.Trim(want, "*`")
		if !strings.Contains(source, plain) {
			t.Errorf("the generator does not draw the note the plugin writes:\n  plugin: %s\n"+
				"  the picture would show something else", plain)
		}
	}
}
