# MediaForge — Specification

> The design record for the plugin: what it does, why it is built this
> way, and which alternatives were rejected.

Plugin ID: `com.github.berzionline.mediaforge`
Repo: `mattermost-plugin-mediaforge`
Short name: MediaForge · Long form: Mattermost MediaForge

---

## 1. Problem

Mattermost has **no native media compression**. Verified in
`server/channels/app/file.go`: the server only derives a 120×100 thumbnail, a
1920 px preview and a 16×16 mini-preview, all at a hard-coded `jpegEncQuality
= 90`. The **original file is stored untouched by design**. Video is never
processed at all — there is no setting, no transcode path, nothing.

The common workaround — a filesystem watcher that rewrites files after upload —
has three structural defects, all observed in production:

1. **It corrupts database metadata.** Mattermost records `Size`, `Width` and
   `Height` in `fileinfo` at upload time. Rewriting the file on disk leaves
   those values stale. Measured on a live instance: **19 of the last 80
   uploads** had mismatched sizes, e.g. DB 313,855,315 vs. actual 31,526,363
   bytes. Consequences: wrong `Content-Length`, aborted downloads, wrong
   storage statistics, wrong preview geometry.
2. **It double-compresses.** The watcher also matches Mattermost's own
   `*_preview.*` files, which were already encoded at quality 90. Re-encoding
   those at quality 90 again, with a second downscale, is the actual cause of
   poor image quality. Observed: 3,499 KB → 157 KB (factor 22).
3. **It cannot hide work in progress.** `inotify` fires *after* the write
   completes, so the post is already delivered. Measured gap on a 306 MB
   video: **2.5 minutes** during which the recipient can fetch a file that is
   being swapped underneath them.

## 2. Approach

Use the official plugin hook `FileWillBeUploaded`, which runs **before** the
file is committed to the backing store:

```go
// FileWillBeUploaded is invoked when a file is uploaded, but before it is
// committed to backing store. [...]
// To reject a file upload, return an non-empty string describing why the file
// was rejected.
// To modify the file, write to the output and/or return a non-nil
// *model.FileInfo, as well as an empty string.
// FileInfo.Size will be automatically set properly if you modify the file.
FileWillBeUploaded(c *Context, info *model.FileInfo, file io.Reader,
                   output io.Writer) (*model.FileInfo, string)
```

This solves all three defects at once:

- **Metadata stays correct** — the server writes `fileinfo` *after* the hook,
  from the modified bytes. Guaranteed by the hook contract quoted above.
- **No double compression** — the hook runs before Mattermost derives its
  thumbnail and preview, so those are generated *from* the compressed original
  and never touched again.
- **Nothing is visible early** — the upload is not complete while the hook
  runs, so the recipient sees nothing until the file is final (matches the
  requested synchronous behaviour).
- **Rejection is supported** — returning a non-empty string fails the upload
  with a message, which is the requested behaviour when video transcoding
  fails.

## 3. Scope

### In scope
- Image compression on upload, configurable.
- Video compression on upload, configurable, synchronous.
- Voice messages: record in web/desktop, post as a regular file attachment.
- Per-feature on/off switches and parameters in the System Console.
- Admin statistics.
- Optional per-file note showing original → compressed size.

### Out of scope (v1)
- Per-channel or filename-based compression exemptions. Deliberately deferred:
  the defaults are to be evaluated in daily use first. The configuration
  surface is designed so this can be added later without a redesign.
- Reprocessing of already-uploaded files.
- Mobile recording UI (not reachable from a server plugin — see §6.3).

## 4. Behaviour

### 4.1 Images

| Aspect | Default | Configurable |
|---|---|---|
| Feature enabled | on | yes |
| Output format | keep original format | yes — keep / always JPEG |
| Max edge length | 2560 px | yes |
| JPEG quality | 85 | yes |
| PNG handling | lossless optimisation, no re-encode to JPEG | yes (via output format) |
| Metadata (EXIF) | preserved | yes — preserve / strip all |
| Minimum size to act on | 512 KB | yes |

Smaller images than the threshold pass through untouched. Images already
below the max edge length are not upscaled.

**Implementation note (added after image compression).** Two rows of this table are
deliberately staged rather than shipped together:

- *Metadata (EXIF)* — image compression implemented the `preserved` default only. The
  `strip all` path arrives with the configuration surface, because before the configuration
  surface exists there is no way for an admin to reach it. Note that EXIF
  routinely contains the GPS coordinates where a photo was taken, so this
  setting is a privacy decision and the System Console text must say so.
- *Output format* — "always JPEG" is **not** implemented and is not scheduled.
  It would convert PNG to JPEG, which is lossy and irreversible, applied to
  the one format users pick precisely because it is lossless (screenshots,
  diagrams, transparency). Adding it needs its own decision, not a checkbox
  added in passing. Until then PNG handling stays at lossless optimisation.

### 4.2 Video

| Aspect | Default | Configurable |
|---|---|---|
| Feature enabled | on | yes |
| Max resolution | 1080p | yes |
| Video codec | H.264 / MP4 | yes — see note below |
| Quality (CRF) | 23 | yes |
| Audio codec | AAC 128 kbit/s stereo | yes |
| On failure | reject upload with error message | no — fixed by decision |

Codec choice is offered with guidance, so an admin can trial a newer codec in
a known-good environment:

- **H.264 / AAC in MP4** — *Recommended. Plays everywhere: Windows, Mac,
  iPhone, Android, all browsers.*
- **H.265 / AAC in MP4** — *About 30 % smaller at equal quality. Firefox and
  older Android devices often cannot play it.*
- **AV1 / Opus in WebM** — *Smallest files. Newer devices and browsers only;
  stutters on older phones. Much slower to encode.*

Above the selector: *"Test a newer format with your team's devices first.
Stay on the recommended format if anyone cannot play videos."*

### 4.3 Voice messages

| Aspect | Default | Configurable |
|---|---|---|
| Feature enabled | on | yes |
| Max duration | 10 min | yes |
| Send immediately on release | no — preview, replay, discard first | yes |
| Waveform display | on | yes |
| Availability | all channels | yes — all / per-channel opt-in |

Audio is posted as a **regular file attachment** (`.m4a`, AAC). This is the
decisive design choice: every client — web, desktop, iOS, Android — plays it
with its built-in audio player, with no client-side plugin support. Plugins
that render a custom player in the webapp work on desktop and stay silent on
mobile; this avoids that class of failure entirely. It also means recordings
remain playable if the plugin is ever disabled.

### 4.4 Cross-cutting

| Aspect | Default | Configurable |
|---|---|---|
| Per-file size note | small footer | yes — five styles, the fifth being "no note" |
| Concurrent video jobs | 1 | yes |
| Concurrent image jobs | 4 | yes |
| Admin statistics | on | — |

**Note contract (important).** When the note is enabled, the *absence* of a
note must reliably mean "this file was stored as the original". This forces
the plugin to record a decision for every file it inspects, including
pass-throughs — otherwise absence carries no information.

This holds **whatever the reason for the pass-through**, including the one
the concurrency limit introduced: a file stored as sent because every processing slot was
busy gets no note either. The sender can do nothing with that information, so
the post stays clean and the server log carries it for the administrator. It is
deliberately not a setting — an option here would let an operator turn the
absence of a note into something ambiguous, which is precisely what this
contract exists to prevent.

**How the note reaches every client** (implemented in the size note). The saving is
recorded at upload time under the file's own ID and collected by
`MessageWillBePosted`, which may modify the post before it is stored. The note
therefore becomes part of the post itself and renders on web, desktop, iOS and
Android with no client-side plugin code — the same reasoning that makes §4.3
post voice messages as plain attachments. A note carried in custom props would
need a webapp component and would stay silent on mobile.

`MessageWillBeUpdated` re-renders it on an edit, so the note neither doubles
nor disappears when a user changes their message. The post carries the two
sizes rather than the finished sentence, which is what lets the note be
re-rendered in whatever style is configured at the time.

**The note is one setting, not two.** `SizeNoteStyle` offers four visible
styles plus "no note", and that fifth option replaces a separate on/off switch:
two ways to reach the same result would only confuse an administrator. The
default is the small footer — the quietest variant, and the only one of the
defaults that does not write into the user's own message.

**Concurrency rationale.** A 1080p transcode saturates all available cores.
Running two in parallel does not halve total time, it doubles both — and makes
Mattermost itself sluggish. Images finish in milliseconds and may run in
parallel. Both values are configurable because other operators run larger
hardware.

**What happens when the limit is reached** (decided in the concurrency limit). The file is
**stored exactly as it was sent** and the upload succeeds; only the compression
is skipped, and a warning naming the file goes to the server log. It is
deliberately not a rejection: that would fail an upload because the *server* was
busy, which the sender can neither understand nor act on, and §4.2's rejection
is reserved for a video the plugin committed to converting and could not.
Waiting without a bound was rejected too — it loses the upload anyway at the
server's write timeout, after the longest possible wait.

**Queue wait and transcode must fit the server's window together** (the concurrency limit).
The two budgets do not compose: the transcode budget (§4.2, default 240 s)
starts when ffmpeg starts, while `ServiceSettings.WriteTimeout` (300 s) started
when the upload did. A job is therefore admitted only if the time still left in
the server's window is enough for a *full* transcode plus the margin to deliver
a rejection — 30 s of queue wait at the defaults. Waiting longer cannot produce
a successful upload, only a slower failure.

Work that reaches no encoder never queues: a pass-through, an audio file in a
video container, and a frugally-encoded video skipped from the header all
consume no processor time and are not made to wait behind a transcode.

## 5. Constraints

- Mattermost server ≥ 9.x (hook available since 5.2; targeting current).
- `ffmpeg` and `ffprobe` must be present for video and audio features. The
  plugin must detect their absence and disable those features with a clear
  admin-visible message rather than failing per upload.
- Reference deployment: 4 vCPU, 6 GB RAM, 1 GB swap.
- Language: Go (server), TypeScript/React (webapp).

### 5.1 Requirement health is shown in the System Console, not only logged

**Decided after the security audit, before the production cutover.** Detection alone is
not enough, and the gap is structural rather than cosmetic: a missing `ffmpeg`
does not fail anything loudly — video compression switches itself off and files
are stored as sent, so images keep working and a broken installation is
indistinguishable from a healthy one. The only signal was one line in the server
log, which an administrator has no reason to read after a successful install.

The plugin therefore reports its own requirements on the page an administrator
is already looking at, measured on request rather than assumed:

- `ffmpeg` and `ffprobe`: the resolved path **and the version**, obtained by
  running the binary. `exec.LookPath` proves a file exists and is executable; it
  does not prove it starts. A wrong architecture or a missing shared library
  passes the lookup and fails every upload.
- The **encoder for the selected codec**, read from `ffmpeg -encoders`. A build
  without `libx265` accepts the H.265 setting and then fails at transcode time.
- The **spool directory**: creatable, writable, and its free space.

**This reports; it does not change behaviour.** A missing encoder is shown in
red and the upload is still rejected exactly as §4.2 requires. Silently storing
videos as sent instead would be a change to §4.2 disguised as a health check,
and §4.2 is a decision that four slices verified.

The display fails **loud**, unlike the voice-settings request which fails closed
by hiding a button: if the health route cannot be reached, the panel shows an
error row. A health display that renders nothing on failure reproduces exactly
the silence it exists to end.

Access is restricted to system administrators, the same as the report. The
answer names absolute server paths, the ffmpeg build in use and free disk space
— the class of internal detail the the security audit audit (Area 4) removed from
user-facing messages. A new route is not an exception to that.

### 5.2 `ffmpeg` is not bundled, and this is settled

Considered and rejected on two independent grounds, both measured rather than
assumed:

- **Licence.** A useful `ffmpeg` is built with `--enable-gpl --enable-libx264
  --enable-libx265` and is therefore **GPL**. MediaForge is Apache-2.0.
  Shipping the binary inside the release would be a licence conflict in a
  repository intended to be public.
- **Size and staleness.** The binary is small (241 KB measured) but drags 210
  shared libraries totalling ~130 MB, per platform. Worse, `ffmpeg` is a media
  parser fed untrusted uploads: it is exactly the component that must receive
  its distribution's security updates. A copy frozen into a plugin that updates
  rarely would age into a liability.

The operating system keeps the dependency current; §5.1 makes its absence
immediately visible, which is what makes relying on the OS safe rather than
merely convenient.

## 6. Known limitations, stated up front

1. **Synchronous video transcoding makes the sender wait.** A 306 MB video
   took 2.5 min on 2 cores; roughly half that on 4. This is the accepted
   trade-off for the "recipient only sees the finished file" requirement.
2. **A failed transcode rejects the upload.** By decision. The user must
   retry or send a smaller file.
3. **Voice recording is web/desktop only.** The plugin can extend the webapp's
   message box, but the mobile apps do not load webapp plugin code. On mobile,
   users record with their phone and attach the file. *Playback* works
   natively everywhere — that is what the attachment-based design buys.

## 7. Verification strategy

Every slice ships with a test that fails before the change and passes after.
The critical invariants, each with its own test:

- **Metadata consistency:** after an upload through the hook, `fileinfo.Size`
  equals the actual byte count on disk. This is the defect that motivates the
  whole project; it gets a dedicated regression test.
- **No preview double-processing:** Mattermost's derived preview is generated
  from the compressed original and is not itself re-processed.
- **Rejection path:** a deliberately corrupt video results in a rejected
  upload with a message, not a stored broken file.
- **Pass-through path:** a file below the threshold is stored byte-identical
  to the input (hash comparison).
- **Note contract:** with notes enabled, every processed file carries a note
  and every passed-through file carries none.
