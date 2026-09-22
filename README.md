# MediaForge

A Mattermost plugin that compresses images and videos **while they are being
uploaded** — before the file is committed to the file store — and lets people
record voice messages in the web and desktop apps.

The point is what that timing buys. Mattermost writes a file's size and
dimensions into its database *after* the upload hook has run, so a file that
MediaForge shrinks is recorded at its real size, its previews are derived from
the already-compressed original, and nobody can fetch a half-written file: the
upload is simply not finished until the compression is.

- Photos and screenshots are scaled and re-saved during upload.
- Videos are converted during upload; the sender waits, and the recipient only
  ever sees the finished file.
- Voice messages are recorded in the browser and posted as an ordinary audio
  attachment, so every client — including the mobile apps — plays them with no
  plugin of its own.
- An optional note on the post tells the sender what was saved.
- An administrator report shows what the plugin has saved overall.

## Contents

- [Why not a filesystem watcher](#why-not-a-filesystem-watcher)
- [Requirements](#requirements)
- [Installing](#installing)
- [Building from source](#building-from-source)
- [Settings](#settings)
- [The report](#the-report)
- [Known limitations](#known-limitations)
- [Troubleshooting](#troubleshooting)
- [Contributing](#contributing)
- [Licence](#licence)

## Why not a filesystem watcher

The common workaround for Mattermost's missing media compression is a script
that watches the file store and rewrites files after the upload. That approach
has three defects, all of them measured on a live instance rather than
theorised:

1. **It corrupts the database.** Mattermost records `Size`, `Width` and
   `Height` at upload time. Rewriting the file afterwards leaves those values
   stale — on one production server, 19 of the last 80 uploads had mismatched
   sizes. The consequences are wrong `Content-Length` headers, aborted
   downloads and wrong storage statistics.
2. **It double-compresses.** The watcher also matches Mattermost's own
   generated `*_preview.*` files, which were already encoded at quality 90.
   Re-encoding those is the actual cause of the poor image quality people
   blame on compression in general.
3. **It cannot hide work in progress.** `inotify` fires *after* the write
   completes, so the post is already delivered. Measured gap on a 306 MB
   video: 2.5 minutes in which a recipient can download a file that is being
   swapped underneath them.

MediaForge uses the official `FileWillBeUploaded` hook instead, which runs
before the file is stored. The full reasoning, including the parts that were
decided against, is in [SPEC.md](SPEC.md).

## Requirements

| | |
|---|---|
| Mattermost Server | 9.0 or newer (tested against 11.10) |
| `ffmpeg` and `ffprobe` | on the server's `PATH`, for video and voice messages |
| Go | 1.27 or newer, only to build from source |
| Node | 20 or newer, only to run the webapp tests |

Image compression needs nothing beyond the plugin itself. Video compression
and voice messages need `ffmpeg` and `ffprobe`; without them those two
features switch themselves off and say so in the server log, rather than
failing one upload at a time. On Debian and Ubuntu:

```sh
sudo apt install ffmpeg
```

**After installing, check the System health panel** at the top of
**System Console → Plugins → MediaForge**. It reports what the server itself
found — the ffmpeg path and version, whether that build carries the encoder your
selected video format needs, and whether the temporary working directory is
writable — so a missing dependency is visible on the page instead of only in
the log.

`ffmpeg` is deliberately **not** bundled with the plugin. It is a media parser
that reads untrusted files, so it needs its distribution's security updates; a
copy frozen inside a plugin release would age badly. Bundling it would also mean
shipping a GPL binary inside an Apache-2.0 release.

Uploading plugins must be allowed on the server
(**System Console → Plugins → Plugin Management → Enable Plugin Uploads**).

## Installing

1. Download or build `mattermost-plugin-mediaforge-<version>.tar.gz`.
2. **System Console → Plugins → Plugin Management → Upload Plugin**, and pick
   the file.
3. Press **Enable** next to MediaForge.
4. Open **System Console → Plugins → MediaForge** and go through the settings
   below. The defaults are usable as they stand; nothing has to be changed to
   start.

Changes take effect on the next upload. The plugin never needs a server
restart, and it never touches files that were uploaded before it was enabled.

To check it is working, post a photo larger than 512 KB in any channel. The
post should carry a small grey footer reading something like
`MediaForge · 11.9 MB → 2.3 MB (80% smaller)`.

## Building from source

```sh
make dist
```

This validates `plugin.json`, cross-compiles the server binary for Linux,
macOS and Windows, assembles the webapp bundle, and writes the installable
archive to `dist/mattermost-plugin-mediaforge-<version>.tar.gz`.

| Target | Purpose |
|---|---|
| `make dist` | builds the installable bundle |
| `make check-style` | `go vet` and `go build` over the whole module |
| `make test` | the Go suite and the webapp tests |
| `make webapp-test` | the webapp tests alone, via `node --test` |
| `make manifest` | validates `plugin.json` on its own |
| `make clean` | removes all build output |

Two things are worth knowing before running the tests:

- **`go test -race` needs `-timeout 40m`.** Under the race detector the server
  package takes about 27 minutes, past Go's default 10-minute per-package
  limit. A bare `go test -race` reports a timeout panic that reads exactly
  like a deadlock and is not one:
  ```sh
  go test ./server -race -count=1 -timeout 40m
  ```
- **The webapp has no dependencies and is meant to keep it that way.** There
  is no `npm install`, no bundler and no `node_modules`: the source is one
  file that takes React from the window the way Mattermost publishes it, and
  the tests run under Node's own test runner. Node 20 or newer works; the
  `make webapp-test` target lets the shell expand its glob so that both Node 20
  and Node 22 run the same 37 tests.

The five preview images in the System Console are generated, not drawn:

```sh
./scripts/make-note-previews.sh
```

It needs `chromium` or `google-chrome` and writes `server/public/note-1.png`
through `note-5.png`. Run it after changing the wording of a note, or the
System Console will show pictures of a note the plugin no longer writes.

## Settings

All of these live under **System Console → Plugins → MediaForge**. Each one
says what it actually does to an upload, which is the only thing worth knowing
about it.

### Images

| Setting | Default | What it does |
|---|---|---|
| **Compress images** | on | The master switch for images. Off means every image is stored exactly as it was sent and none of the settings below has any effect. |
| **Longest edge (pixels)** | 2560 | Images larger than this are scaled down until neither side is longer. Smaller images are never enlarged. Below roughly 1000, text in screenshots starts to become hard to read. |
| **Photo quality (JPEG)** | 85 | How much detail to keep when re-saving a photo, 1 to 100. 85 is what most people cannot tell apart from the original. Below about 70 the loss shows as blocky patches in skies. PNG files are unaffected — they are always re-saved without loss. |
| **Only compress files above (KB)** | 512 | Images smaller than this are stored untouched. Small images save almost nothing and can even grow when re-saved. |
| **Photo information (EXIF)** | Keep | Phone photos usually carry the exact location they were taken at, readable by anyone who downloads the file. **Keep** leaves it in place and keeps colour profiles intact; **Remove** strips all of it. This is a privacy decision, not a storage one. |

PNG is never converted to JPEG. That would be a lossy, irreversible conversion
applied to the one format people choose precisely because it is lossless, so
it is deliberately not offered.

### Videos

Video compression needs `ffmpeg` and `ffprobe`.

| Setting | Default | What it does |
|---|---|---|
| **Compress videos** | on | The master switch for videos. The sender waits while the conversion runs, which is what stops anybody from opening a half-written file. |
| **Largest video height (pixels)** | 1080 | Taller videos are scaled down to this; the width follows so nothing is distorted. 720 roughly halves the file again and is hard to tell apart on a phone. |
| **Video format** | H.264 in MP4 | H.264 plays everywhere. H.265 is about 30% smaller but not every device plays it; AV1 is smaller still and only recent devices play it. Test a newer format with your own team's devices before switching everyone over — a stored video nobody's phone will play cannot be recovered without uploading it again. |
| **Video quality** | 23 | Counts *downwards*: lower means better picture and a larger file. 18 is practically identical to the source; 28 is visibly softer but fine for screen recordings. |
| **Sound quality (kbit/s)** | 128 | Sound is a small part of a video file, so lowering this saves much less than lowering the picture quality. |
| **Give up converting after (seconds)** | 240 | A conversion that runs longer is abandoned and the upload is refused. Keep this a little below your server's **Environment → Web Server → Write Timeout** (300 s by default) — past that point the upload is lost anyway, and the time is spent on a file nobody can receive. |
| **Do not convert videos larger than (MB)** | 2048 | Videos above this are stored as sent rather than refused. It is a limit on temporary disk space, not on what people may send: each conversion briefly needs about twice the video's size free in the server's temporary directory. |

A video whose conversion fails is **refused**, with a message to the sender.
That is deliberate: storing a broken video quietly would be worse.

### Voice messages

Voice messages need `ffmpeg` and `ffprobe`.

| Setting | Default | What it does |
|---|---|---|
| **Voice messages** | on | Adds a microphone entry to the attachment menu in the web and desktop apps. The recording is converted on the server to `.m4a` and posted as an ordinary audio attachment, so every client plays it — including the mobile apps, which never load plugin code. |
| **Longest voice message (seconds)** | 600 | The browser stops recording when it reaches this, so nobody talks past the limit and loses what they said. A longer recording sent by some other client is refused with a message naming both lengths. |
| **Shorten long pauses in voice messages** | off | Shortens the silent gaps inside a recording while it is converted. Measured on a recording with ordinary pauses: 12.5 s became 8.8 s, at about 7 s of server time for a ten-minute file. **The only setting here that is off by default, and deliberately so:** everything else MediaForge does re-encodes a file, while this removes part of what somebody said and cannot be undone. Pauses are shortened rather than deleted — about four tenths of a second is left standing, so sentences do not run together and nobody sounds breathless — and the start of a recording is never trimmed, so a quietly spoken first word is not clipped. |
| **Channels with voice messages** | empty | Empty means everywhere. Otherwise list channels by URL name (`town-square`), display name (`Town Square`) or ID, separated by commas. Elsewhere the microphone button is not shown, and a recording sent there anyway is stored as an ordinary audio file rather than converted. Nothing is lost either way. |

Every voice message carries its playing time on the post, as `🎤 Voice
message · 2:05`. It is the length of the file that was **stored**, which is
what a reader deciding whether to listen actually faces — when pauses have
been shortened, that is shorter than what was spoken.

That is on the post rather than in a player component for the
same reason recordings are ordinary attachments: it has to be readable on a
phone, and the mobile apps load no plugin code.

### The note on the post

| Setting | Default | What it does |
|---|---|---|
| **Size note on the post** | A small, quiet footer | Shows the sender what a file's compression saved. Five options, illustrated by the five numbered pictures under the setting. |

The five options are: **1** an italic line under the message, **2** just the
two sizes in a small box, **3** a separate block with a green bar, **4** a
small quiet footer, and **5** no note at all.

Option 5 is how the note is switched off — there is deliberately no separate
on/off switch, because two ways to reach the same result only confuse.

**The absence of a note means something**, and that is the reason the feature
is built the way it is: a file that carries no note was stored exactly as it
was sent. That holds whatever the reason — too small to be worth compressing,
a format MediaForge does not touch, or the server being busy at that moment.
If you turn the note off and on again later, posts made while it was off get
their notes the next time they are edited, so the promise does not depend on
when a file happened to be sent.

The pictures under the setting are numbered 1 to 5 down their left edge.
Mattermost renders a radio option as plain text and puts the help text as one
block below the whole group, so a picture cannot sit next to its own option —
the number is what pairs them.

## The report

**System Console → Plugins → MediaForge**, at the bottom of the settings page:
*Open the MediaForge report*. Or directly:

```
https://<your-server>/plugins/com.github.berzionline.mediaforge/report
```

It shows how many files were made smaller, how much storage that saved, and
what could not be converted — split into images and videos, and day by day.
Only system administrators can open it: an ordinary logged-in user gets 403,
and someone who is not signed in at all gets 401.

## Known limitations

These are stated up front because they are consequences of the design rather
than defects waiting to be fixed.

1. **Video conversion makes the sender wait.** A 306 MB video takes about 2.5
   minutes on two cores, roughly half that on four. This is the price of the
   guarantee that a recipient never sees a half-finished file.
2. **A failed conversion refuses the upload.** The sender is told, and has to
   retry or send something smaller. Storing a video that failed to convert
   would mean storing something that may not play.
3. **Voice *recording* is web and desktop only.** The mobile apps do not load
   webapp plugin code, so the microphone button cannot appear there; on a
   phone, people record with the phone and attach the file. *Playback* works
   everywhere — that is exactly what the attachment-based design buys.
4. **Already-uploaded files are never touched.** MediaForge acts during
   upload and only during upload. There is no batch job for your existing
   file store.
5. **Everything is in English.** There is no translation layer yet; using
   Mattermost's `i18n` machinery is its own piece of work.
6. **A recording's file on disk keeps the browser's `.webm` suffix** while the
   database correctly says `.m4a`. Nothing a user can see depends on it —
   playback is driven by the recorded MIME type and name — and correcting it
   properly means deleting the old path after the server has moved the file,
   which is a change to Mattermost's own upload code rather than to this
   plugin.

## Troubleshooting

### Videos are not being compressed

**Look at the System health panel first.** It sits at the top of
**System Console → Plugins → MediaForge** and is checked on the server every
time you open the page. It names the ffmpeg it found, its version, whether that
build has the encoder your selected video format needs, and whether the
temporary working directory is writable with room to spare. A red row there
names the cause and what to do about it, which is faster than reading logs.

Almost always `ffmpeg` or `ffprobe` is missing from the server's `PATH`.
MediaForge checks for both whenever its settings are applied and writes a
warning to the server log naming what it could not find:

```
MediaForge: video compression is switched on, but ffmpeg and ffprobe not found
in the server's PATH, so video compression is switched off and videos are
stored exactly as they were sent. Install ffmpeg and ffprobe on the server and
save this page again.
```

Check it on the server itself, as the user Mattermost runs as:

```sh
sudo -u mattermost which ffmpeg ffprobe
```

A common trap is that `ffmpeg` exists for your own shell but not for the
service: a systemd unit does not inherit an interactive `PATH`. If `which`
finds it as you but the log still complains, that is what is happening.

After installing it, **press Save on the MediaForge settings page**. The lookup
runs every time the settings are applied, so saving is enough — the plugin does
not need to be disabled and re-enabled, and the server does not need a restart.

The second possibility is the settings rather than the toolchain: **Compress
videos** switched off, or the video being larger than **Do not convert videos
larger than**, in which case it is stored as sent on purpose.

### There is no microphone button

Work through these in order:

1. **Is it a web or desktop client?** The mobile apps never show it, by
   design. This is limitation 3 above, not a fault.
2. **Is `ffmpeg` installed?** Voice messages need it for the same reason
   videos do, and the button is hidden rather than offered on a server that
   cannot convert. See the previous section.
3. **Is the setting on?** **Voice messages** in the System Console.
4. **Is this channel in the list?** If **Channels with voice messages** is not
   empty, the button only appears in the channels it names. Empty means
   everywhere.
5. **Reload the browser.** The webapp asks the server once per page load
   whether to draw the button, so a setting changed while a tab was open does
   not reach that tab until it reloads.

You can ask the server what it answers for a given channel, signed in as any
user:

```
GET /plugins/com.github.berzionline.mediaforge/voice/settings?channel_id=<id>
{"enabled":true,"name_prefix":"mediaforge-voice-","max_seconds":600}
```

`enabled: false` with a `name_prefix` present means the feature works but not
in that channel. No `name_prefix` at all means the server cannot do voice
messages — back to `ffmpeg`.

### A video is not getting smaller

MediaForge deliberately does not re-encode a video it cannot improve. A clip
that is already efficiently encoded, or already below the height limit at a
modest bitrate, is stored as sent — re-encoding it would cost processor time
and lose quality for nothing.

The other cases, in the order worth checking:

- **The file is above the size limit.** Above **Do not convert videos larger
  than**, videos pass through untouched.
- **Two people uploaded at once.** With **Convert this many videos at the same
  time** at its default of 1, a video arriving while another is being
  converted is stored as sent rather than made to wait. The server log names
  the file when this happens.
- **The conversion ran out of time.** Then the upload is *refused* rather than
  stored — if the file is on the server, this is not what happened.

A file that was stored as sent carries no note. That is the fastest way to
tell: a note means it was compressed and says by how much; no note means the
stored file is the original.

### An upload was refused

Two things refuse an upload, and both tell the sender why:

- a video whose conversion failed or ran past **Give up converting after**;
- a voice recording longer than **Longest voice message**, or one the server
  could not read at all.

Nothing else does. Image compression never fails an upload: if an image cannot
be processed for any reason, it is stored exactly as it was sent.

### The settings page shows broken images

The five preview pictures are served by the plugin itself from
`/plugins/com.github.berzionline.mediaforge/images/note-1.png` and so on. If
they do not appear, check that path directly in a browser — it should return
a PNG. If it returns 404, the installed bundle is older than the settings page
expects; rebuild and reinstall it.

### Nothing is compressed at all

Check that the plugin is actually enabled (**Plugin Management**), and that
the server log carries `MediaForge activated` from the moment it started. If
an image smaller than 512 KB was the test, that is the
**Only compress files above (KB)** threshold doing its job — try a photo
straight from a phone.

## Contributing

Issues and pull requests are welcome at
[BerziOnline/mattermost-plugin-mediaforge](https://github.com/BerziOnline/mattermost-plugin-mediaforge).

Please read [SPEC.md](SPEC.md) first. It records the design decisions and the
measurements behind them, including the alternatives that were rejected and
why, so they do not have to be re-argued per pull request.

Two conventions the codebase holds to:

- **Every change ships with a test that failed before it.** The plugin sits on
  the upload path of every file, so behaviour that is not pinned by a test is
  behaviour nobody will notice breaking.
- **All code, comments and user-visible text are in English**, and the tests
  in `server/language_test.go` enforce the last of those.

## Licence

[Apache License 2.0](LICENSE)
