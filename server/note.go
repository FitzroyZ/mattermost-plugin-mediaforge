package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// The five values the SizeNoteStyle setting offers (PLAN §the size note). They are
// numbered 1 to 5 in the System Console, and the five preview images carry the
// same number, because a radio option renders as plain text and cannot show a
// picture of its own.
const (
	// noteStyleLine appends an italic line under the user's own message.
	noteStyleLine = "line"
	// noteStyleCompact appends just the two numbers as inline code.
	noteStyleCompact = "compact"
	// noteStyleBlock adds a separated block with a coloured bar.
	noteStyleBlock = "block"
	// noteStyleFooter adds a small, quiet footer. This is the default.
	noteStyleFooter = "footer"
	// noteStyleNone shows nothing at all. It replaces a separate on/off
	// switch: two ways to reach the same result would only confuse an
	// administrator, so there is deliberately no EnableSizeNote.
	noteStyleNone = "none"
)

// notePropKey carries the plugin's own bookkeeping on the post.
//
// It records the sizes rather than the rendered sentence, which is what makes
// an edit safe in both directions: the note can be stripped in whatever style
// it was written in, and re-rendered in whatever style is configured now.
//
// It is bookkeeping, NOT the note itself. The note has to live in the message
// or in a message attachment, because those are the only two things every
// client already knows how to display — including the mobile apps, which never
// load plugin code. A note that lived only in a custom prop would need a
// webapp component and would stay silent on a phone, which is exactly the
// class of failure SPEC §4.3 avoids for voice messages.
const notePropKey = "mediaforge_sizes"

// noteAttachmentFallback marks the attachment as the plugin's own.
//
// Posts may carry attachments from other plugins or from incoming webhooks, so
// an edit must be able to tell which one it owns. It doubles as the text a
// client without attachment support shows, which is why it reads as a sentence.
const noteAttachmentFallback = "MediaForge"

// noteKeyPrefix namespaces the records in the key-value store.
const noteKeyPrefix = "mfnote_"

// maxNoteRetention bounds how long a recorded saving is kept.
//
// The record is written when a file is uploaded and collected when the post
// arrives, usually seconds later. But a draft can be abandoned, an upload
// cancelled and a client closed, and none of those ever produce the post that
// would collect it. Without an expiry every one of them would stay for ever,
// so the store would grow without bound for as long as the plugin is
// installed.
//
// A day is long enough for the one case that legitimately takes time — a draft
// with an attachment that somebody leaves open overnight — and short enough
// that abandoned records cost nothing.
const maxNoteRetention = 24 * time.Hour

// noteRecord is what one compressed file contributes to the post.
type noteRecord struct {
	// Name is the file name, kept for diagnosis rather than display. The note
	// itself does not name the file: a post shows its attachments already.
	Name string `json:"name"`
	// Original is the size of the file the user sent.
	Original int64 `json:"original"`
	// Stored is the size of the file on the server.
	Stored int64 `json:"stored"`
	// Seconds is how long the recording plays, for a voice message, and zero
	// for everything else.
	//
	// It answers the complaint that an audio attachment tells a reader
	// nothing about what it is asking of them: the length only becomes
	// visible once it is already playing, which is too late to decide to
	// listen later. The number is measured anyway — the duration probe reads
	// it to enforce MaxVoiceSeconds — and was previously discarded.
	//
	// It rides on the note rather than on a webapp component for the reason
	// that governs this whole plugin: the mobile apps load no plugin code, so
	// anything a phone must see has to be part of the post itself.
	//
	// `omitempty` keeps the stored record identical for every non-voice file,
	// so nothing that already exists changes shape.
	Seconds float64 `json:"seconds,omitempty"`
}

// recordSaving stores what one upload saved, under the file's own ID.
//
// The ID is the link between the two hooks: the upload returns it and the post
// created afterwards references exactly it, so the numbers of one file can
// never end up on another file's post — verified against a running server
// rather than inferred from the documentation (PLAN §the size note).
//
// A failure here is logged rather than swallowed. The SPEC §4.4 contract makes
// the ABSENCE of a note meaningful, so a note that silently fails to appear
// would quietly weaken the very promise this slice exists to make. It must
// never fail the upload, though: storing the user's file is the job, and the
// note is a convenience on top of it.
func (p *MediaForgePlugin) recordSaving(info *model.FileInfo, outcome processOutcome) {
	if p.API == nil {
		return
	}

	// Two different reasons to write a record, and they must not be merged.
	//
	// A COMPRESSION records what it saved. A converted VOICE recording
	// records only how long it plays — deliberately with no sizes at all,
	// because a conversion from Opus to AAC usually makes the file larger.
	// Reporting that as a saving would put a growth into the administrator's
	// report as though it were a compression, which is the one figure the
	// statistics exist to keep honest.
	//
	// Before this, the early return on !reEncoded meant a recording got no
	// record whatsoever, so its length had nowhere to travel.
	record := noteRecord{Name: info.Name}
	switch {
	case outcome.reEncoded:
		if outcome.originalSize <= 0 || outcome.bytesWritten <= 0 {
			return
		}
		record.Original = outcome.originalSize
		record.Stored = outcome.bytesWritten
		record.Seconds = outcome.spokenSeconds
	case outcome.voiceConverted && outcome.spokenSeconds > 0:
		record.Seconds = outcome.spokenSeconds
	default:
		// Stored exactly as sent. No record, so the post carries no note —
		// which is the promise that an absent note means an untouched file.
		return
	}

	encoded, err := json.Marshal(record)
	if err != nil {
		p.logWarn("MediaForge: could not prepare the size note for this file, so the post will "+
			"carry no note for it",
			"file_id", info.Id, "file_name", info.Name, "error", err.Error())
		return
	}

	if appErr := p.API.KVSetWithExpiry(
		noteKeyPrefix+info.Id, encoded, int64(maxNoteRetention/time.Second),
	); appErr != nil {
		p.logWarn("MediaForge: could not record the size note for this file, so the post will "+
			"carry no note for it although the file was compressed",
			"file_id", info.Id, "file_name", info.Name, "error", appErr.Error())
	}
}

// collectSavings reads the records belonging to a post's files.
//
// It deliberately does NOT delete them. Deleting on read is the obvious
// design and it is wrong here, because this hook runs BEFORE the post is
// committed and is not the last word on whether it ever will be: a later
// plugin may reject the post, and the save itself can fail. A record consumed
// by an attempt that came to nothing would leave the user's retry without a
// note although the file was compressed — an exception to the SPEC §4.4
// contract that appears only under a condition nobody reproduces by hand.
//
// The expiry is what cleans up instead. It has to exist for abandoned drafts
// anyway, so letting it cover this case as well costs one key for at most a
// day and removes a whole failure mode.
func (p *MediaForgePlugin) collectSavings(fileIDs []string) []noteRecord {
	if p.API == nil {
		return nil
	}

	var records []noteRecord
	for _, id := range fileIDs {
		key := noteKeyPrefix + id
		raw, appErr := p.API.KVGet(key)
		if appErr != nil || len(raw) == 0 {
			// No record means the file was stored as the original. That is
			// the normal case for most uploads and is not an error.
			continue
		}
		var record noteRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			p.logWarn("MediaForge: a recorded size note could not be read back and was discarded",
				"file_id", id, "error", err.Error())
			// A record that cannot be parsed will never become a note, so it
			// is removed rather than left to occupy a key for a day.
			_ = p.API.KVDelete(key)
			continue
		}
		records = append(records, record)
	}
	return records
}

// MessageWillBePosted adds the size note to a post before it is stored.
//
// This hook is what makes the note appear on every client without any of them
// knowing MediaForge exists: the note becomes part of the post itself, so web,
// desktop, iOS and Android all render it with what they already have.
func (p *MediaForgePlugin) MessageWillBePosted(_ *plugin.Context, post *model.Post) (*model.Post, string) {
	if post == nil || len(post.FileIds) == 0 {
		// The overwhelming majority of posts carry no file. For them the
		// plugin must be entirely invisible — no message change, no prop.
		return nil, ""
	}

	records := p.collectSavings(post.FileIds)
	if len(records) == 0 {
		return nil, ""
	}

	updated := post.Clone()
	p.applyNote(updated, records)
	return updated, ""
}

// MessageWillBeUpdated keeps the note correct when a post is edited.
//
// This is the awkward half of writing into someone else's message. The client
// sends back the stored message with the user's change applied, note included,
// so appending again would double it — and a user who deleted the line by hand
// would otherwise lose it for good. Both outcomes are visible, and both would
// make the note untrustworthy: a note that is sometimes there and sometimes
// not says nothing by its absence, which is the one thing SPEC §4.4 forbids.
//
// The sizes are read from the STORED post rather than from the key-value
// store, because the record was collected and deleted when the post was first
// created. That is also what makes an edit years later still render correctly.
func (p *MediaForgePlugin) MessageWillBeUpdated(
	_ *plugin.Context, newPost, oldPost *model.Post,
) (*model.Post, string) {
	// Unlike MessageWillBePosted, a nil post here is a REJECTION: Mattermost
	// answers 400 "Post rejected by plugin." and keeps the old message. Every
	// edit we do not need to touch must therefore hand back newPost itself.
	if newPost == nil || oldPost == nil {
		return newPost, ""
	}

	records := recordsFromProps(oldPost)
	if len(records) == 0 {
		return newPost, ""
	}

	updated := newPost.Clone()
	p.applyNote(updated, records)
	return updated, ""
}

// applyNote renders the note onto a post, replacing whatever was there before.
//
// It is written to be idempotent: applying it twice produces the same post as
// applying it once. That property is what the edit path depends on, and it is
// cheaper to guarantee here than to reason about at each call site.
func (p *MediaForgePlugin) applyNote(post *model.Post, records []noteRecord) {
	style := p.getConfiguration().SizeNoteStyle

	post.Message = stripNoteLines(post.Message, records)
	setAttachments(post, withoutOurAttachments(attachmentsOf(post)))

	// The bookkeeping prop is written whatever the style, including "none".
	// An administrator who switches the note back on must get it on posts
	// that were created while it was off — otherwise the absence of a note
	// would mean two different things depending on when the file was sent.
	post.AddProp(notePropKey, encodeRecords(records))

	switch style {
	case noteStyleLine, noteStyleCompact:
		if line := messageNote(style, records); line != "" {
			post.Message = strings.TrimRight(post.Message, "\n") + "\n" + line
		}
	case noteStyleBlock, noteStyleFooter:
		if attachment := attachmentNote(style, records); attachment != nil {
			setAttachments(post, append(attachmentsOf(post), attachment))
		}
	case noteStyleNone:
		// Nothing to show. The stripping above already removed a note left
		// over from a time when the setting was different.
	}
}

// messageNote renders the styles that append to the user's own message.
func messageNote(style string, records []noteRecord) string {
	var lines []string
	for _, r := range records {
		length := clockSuffix(r.Seconds)
		if r.Original <= 0 || r.Stored <= 0 {
			// A record with no sizes is a voice recording: it reports how long
			// it plays and nothing else, because the conversion usually GREW
			// the file. Rendering the size half anyway would print "0 B → 0 B"
			// or, worse, a negative saving.
			if length != "" {
				lines = append(lines, voiceOnlyNote(style, length))
			}
			continue
		}
		switch style {
		case noteStyleLine:
			lines = append(lines, fmt.Sprintf("*MediaForge: %s → %s (%d%% saved)%s*",
				formatFileSize(r.Original), formatFileSize(r.Stored),
				savedPercent(r.Original, r.Stored), length))
		case noteStyleCompact:
			lines = append(lines, fmt.Sprintf("`%s → %s%s`",
				formatFileSize(r.Original), formatFileSize(r.Stored), length))
		}
	}
	return strings.Join(lines, "\n")
}

// attachmentNote renders the styles that sit beside the message rather than
// inside it.
//
// These two are the reason the note does not have to touch the user's words at
// all. The default is the footer, which is the quietest of the four.
func attachmentNote(style string, records []noteRecord) *model.MessageAttachment {
	var lines []string
	for _, r := range records {
		length := clockSuffix(r.Seconds)
		if r.Original <= 0 || r.Stored <= 0 {
			if length != "" {
				lines = append(lines, voiceOnlyNote(style, length))
			}
			continue
		}
		switch style {
		case noteStyleBlock:
			lines = append(lines, fmt.Sprintf("📦 %s → %s · %d%% saved by MediaForge%s",
				formatFileSize(r.Original), formatFileSize(r.Stored),
				savedPercent(r.Original, r.Stored), length))
		case noteStyleFooter:
			lines = append(lines, fmt.Sprintf("MediaForge · %s → %s (%d%% smaller)%s",
				formatFileSize(r.Original), formatFileSize(r.Stored),
				savedPercent(r.Original, r.Stored), length))
		}
	}
	if len(lines) == 0 {
		return nil
	}

	text := strings.Join(lines, "\n")
	attachment := &model.MessageAttachment{
		Fallback: noteAttachmentFallback,
		Text:     text,
	}
	switch style {
	case noteStyleBlock:
		// The green bar of the approved preview.
		attachment.Color = "#4fba85"
	case noteStyleFooter:
		// A grey bar, quiet enough not to compete with the message.
		attachment.Color = "#d9d9db"
	}
	return attachment
}

// stripNoteLines removes any note the plugin could have written for these
// records, in any of its styles.
//
// Matching against the renderings the plugin itself would produce — rather
// than against anything that merely looks like a size — is what keeps it from
// deleting a user's own line that happens to contain an arrow.
func stripNoteLines(message string, records []noteRecord) string {
	known := map[string]bool{}
	for _, style := range []string{noteStyleLine, noteStyleCompact} {
		for _, r := range records {
			if line := messageNote(style, []noteRecord{r}); line != "" {
				known[line] = true
			}
			// And the note the German build wrote, before the documentation pass translated
			// these renderings. A post that carries one predates the upgrade,
			// and the current renderings no longer match it — so without this
			// an edit would leave the old note in place and append a second,
			// English one beside it. Two notes on one file is precisely what
			// the SPEC §4.4 contract forbids, and it would show up only when
			// somebody edits an old post.
			if line := legacyGermanNote(style, []noteRecord{r}); line != "" {
				known[line] = true
			}
		}
	}

	lines := strings.Split(message, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if known[strings.TrimSpace(line)] {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n")
}

// attachmentsOf reads the attachments already on a post.
//
// The prop arrives as whatever the client sent, so it is round-tripped through
// JSON rather than type-asserted: an edit from the webapp delivers
// []any of map[string]any, while a post the plugin built itself delivers the
// typed slice. Assuming either shape would work in tests and fail in
// production.
func attachmentsOf(post *model.Post) []*model.MessageAttachment {
	if post.Props == nil {
		return nil
	}
	raw, ok := post.Props["attachments"]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var attachments []*model.MessageAttachment
	if err := json.Unmarshal(encoded, &attachments); err != nil {
		return nil
	}
	return attachments
}

// withoutOurAttachments drops the plugin's own attachment and keeps everyone
// else's.
func withoutOurAttachments(attachments []*model.MessageAttachment) []*model.MessageAttachment {
	kept := make([]*model.MessageAttachment, 0, len(attachments))
	for _, a := range attachments {
		if a != nil && a.Fallback == noteAttachmentFallback {
			continue
		}
		kept = append(kept, a)
	}
	return kept
}

// setAttachments writes the attachments back, removing the prop entirely when
// none are left.
//
// Leaving an empty list behind would be a visible difference from a post the
// plugin never touched, and "the plugin is invisible unless it has something
// to say" is worth more than one saved line of code.
func setAttachments(post *model.Post, attachments []*model.MessageAttachment) {
	if len(attachments) == 0 {
		if post.Props != nil {
			post.DelProp("attachments")
		}
		return
	}
	post.AddProp("attachments", attachments)
}

// encodeRecords renders the bookkeeping prop.
//
// It is a string rather than a nested object because post props cross the
// wire, the database and several clients, and a string survives all of them
// unchanged. A nested object would come back as map[string]any with its
// numbers turned into float64.
func encodeRecords(records []noteRecord) string {
	encoded, err := json.Marshal(records)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// recordsFromProps reads the bookkeeping prop back off a stored post.
func recordsFromProps(post *model.Post) []noteRecord {
	if post.Props == nil {
		return nil
	}
	raw, ok := post.Props[notePropKey]
	if !ok {
		return nil
	}
	encoded, isString := raw.(string)
	if !isString || encoded == "" {
		return nil
	}
	var records []noteRecord
	if err := json.Unmarshal([]byte(encoded), &records); err != nil {
		return nil
	}
	return records
}

// formatFileSize writes a byte count the way the preview images show it:
// decimal units with an English decimal point, as in "11.9 MB".
//
// Decimal rather than binary units on purpose. The note is read by the person
// who sent the file, and every phone, camera and operating system they see the
// same file in counts a megabyte as a million bytes. Using 1024 here would
// make the plugin disagree with the file manager the user checks it against.
//
// The separator was a comma until the documentation pass, because the note was first written
// in German. Every user-visible text is English now, and English spells a
// decimal separator as a point. `TestNoSizeIsWrittenWithGermanNumberPunctuation`
// is the guard, because nothing else in the plugin would notice the comma
// coming back.
func formatFileSize(bytes int64) string {
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

		// Kilobytes are shown whole: a tenth of a kilobyte is noise, and the
		// design writes "340 KB", not "340.0 KB".
		if unit == "KB" {
			rounded := math.Round(value)
			if rounded >= 1000 {
				// Rounding pushed it into the next unit, e.g. 999 950 bytes.
				continue
			}
			return fmt.Sprintf("%d KB", int64(rounded))
		}

		rounded := math.Round(value*10) / 10
		if rounded >= 1000 && i < len(units)-1 {
			continue
		}
		return strconv.FormatFloat(rounded, 'f', 1, 64) + " " + unit
	}
	return fmt.Sprintf("%d B", bytes)
}

// savedPercent is how much smaller the stored file is, in whole percent.
//
// It rounds down, so the note never claims a saving larger than the one that
// actually happened.
func savedPercent(original, stored int64) int {
	if original <= 0 || stored >= original {
		return 0
	}
	return int((original - stored) * 100 / original)
}

// clockLength renders a spoken length as m:ss, the way every audio player
// shows it.
//
// Deliberately NOT humanSeconds ("2 minutes 5 seconds"), which reads well in a
// sentence addressed to somebody whose recording was just refused. This one
// sits next to an attachment and is scanned rather than read, so it uses the
// notation a reader already knows from the play button.
//
// Zero means "not a recording, or never measured" and renders as nothing at
// all. That matters more than it looks: every compressed photograph goes
// through the same renderer, and a photograph with a playing time of 0:00
// beside it reads like a defect.
//
// It rounds UP to one second rather than to the nearest, because the nearest
// produces 0:00 for anything under half a second — a clock reading "no time"
// next to an attachment that plainly contains speech. Minutes are not carried
// into hours: a recording long enough for that is already pathological, and
// "61:01" is unambiguous where "1:01:01" invites being misread as one minute.
func clockLength(seconds float64) string {
	if seconds <= 0 {
		return ""
	}
	whole := int(seconds + 0.5)
	if whole < 1 {
		whole = 1
	}
	return fmt.Sprintf("%d:%02d", whole/60, whole%60)
}

// clockSuffix is clockLength ready to be appended to a note, or the empty
// string when there is no length to show.
func clockSuffix(seconds float64) string {
	length := clockLength(seconds)
	if length == "" {
		return ""
	}
	return " · " + length
}

// voiceOnlyNote renders the note for a file that has a playing time but no
// saving to report.
//
// `length` arrives as the " · m:ss" suffix the size notes append, so it is
// trimmed here rather than formatted twice.
//
// The wording says what the attachment IS, not what was done to it. A reader
// scanning a channel wants to know that this is a recording and how much of
// their time it wants; that MediaForge converted it is the plugin's business.
func voiceOnlyNote(style, length string) string {
	clock := strings.TrimPrefix(length, " · ")
	switch style {
	case noteStyleLine:
		return fmt.Sprintf("*🎤 Voice message · %s*", clock)
	case noteStyleCompact:
		return fmt.Sprintf("`🎤 %s`", clock)
	default:
		// Block and footer both sit beside the message rather than inside it.
		return fmt.Sprintf("🎤 Voice message · %s", clock)
	}
}
