#!/bin/sh
# make-note-previews.sh — rebuilds the five System Console preview images.
#
# The SizeNoteStyle setting is a radio with five options, and Mattermost renders
# a radio option as plain text: no Markdown, no image. The help_text below the
# whole group is the only place a picture can go, so the five variants are shown
# as five pictures in a list, each carrying the number of the option it belongs
# to down its left edge. That number is the ONLY thing pairing an option with
# its picture — which is why it has to stay.
#
# earlier work shipped images that had been produced once, by hand, with
# nothing recording how; the documentation pass had to rebuild all five from scratch. This
# script exists so that does not happen a second time.
#
# Requirements: chromium or google-chrome, plus the fonts the result should be
# set in — Open Sans for the text and Noto Color Emoji for the 📦 of variant 3.
# Neither is a runtime dependency of the plugin: this is a build tool in the
# same sense as gosec, and it must never enter go.mod.
#
# Usage:  ./scripts/make-note-previews.sh [output-directory]
# Default output directory: server/public
#
# Afterwards:  go test ./server -run TestThePreviewImages -v
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/.." && pwd)
out=${1:-$root/server/public}

browser=""
for candidate in chromium chromium-browser google-chrome google-chrome-stable; do
	if command -v "$candidate" >/dev/null 2>&1; then
		browser=$candidate
		break
	fi
done
if [ -z "$browser" ]; then
	echo "no chromium or chrome found; install one to rebuild the previews" >&2
	exit 1
fi

# Chromium refuses to start its sandbox as root and then exits without a word:
# the screenshot simply never appears. A container is the normal place this
# runs, so the flag is added where it is needed rather than always.
sandbox=""
if [ "$(id -u)" = "0" ]; then
	sandbox="--no-sandbox"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

mkdir -p "$out"

# The five example posts. Each shows a message the way a user would write it,
# the note in the style being illustrated, and the photo the note is about.
#
# The wording is English throughout, as every user-visible text has been since
# the documentation pass. The numbers are the ones from the the size note server proof — an 11.9 MB
# photo stored at 2.3 MB — so the picture shows a saving that really happened.
#
# The height is MEASURED, not chosen. A headless screenshot is a picture of the
# window, so a window one line too short crops the image in silence — which the
# first draft of this script did to three of the five variants, cutting the
# photo off at the bottom edge where it looked like a design decision. The page
# is therefore rendered once to ask the browser how tall it came out, and
# photographed at that height on a second pass. That also survives a machine
# with different fonts, which lays the text out slightly differently and would
# otherwise lose the last line.
# shellcheck disable=SC2016
render() {
	number=$1
	note=$2

	cat >"$work/note-$number.html" <<HTML
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<style>
  /* The page IS the image: no margins, and a width of exactly the size the
     screenshot is taken at, so nothing has to be cropped afterwards. */
  html, body {
    margin: 0;
    padding: 0;
    width: 410px;
    background: #f5f6f8;
  }
  body {
    display: flex;
    font-family: "Open Sans", "Noto Sans", sans-serif;
    -webkit-font-smoothing: antialiased;
  }

  /* The number column. It is what pairs this picture with option $number in
     the System Console, so after the note itself it is deliberately the
     strongest thing on the image. */
  .number {
    width: 33px;
    flex: none;
    padding-top: 9px;
    text-align: center;
    font-size: 15px;
    font-weight: 700;
    color: #5d6169;
  }

  /* The post, as Mattermost's light theme draws it. */
  .post {
    flex: 1;
    background: #ffffff;
    border: 1px solid #d6d8dc;
    border-radius: 4px;
    padding: 8px 10px 10px;
    box-sizing: border-box;
  }
  .message {
    font-size: 13.5px;
    line-height: 18px;
    color: #3f4350;
  }

  /* Variant 1 — an italic line under the message. */
  .line {
    font-size: 12px;
    line-height: 17px;
    font-style: italic;
    color: #8b8d94;
    margin-top: 1px;
  }

  /* Variant 2 — just the two numbers, as inline code. */
  .compact {
    display: inline-block;
    margin-top: 3px;
    padding: 1px 5px;
    border: 1px solid #e0e0e2;
    border-radius: 4px;
    background: #f7f7f9;
    font-family: "Liberation Mono", "DejaVu Sans Mono", monospace;
    font-size: 11.5px;
    color: #5d6169;
  }

  /* Variants 3 and 4 — a message attachment, a coloured bar and a line of text
     beside the message rather than inside it. The two differ in the colour of
     the bar and in the wording, which is the whole point of showing both. */
  .attachment {
    display: flex;
    margin-top: 7px;
    border-radius: 3px;
    overflow: hidden;
    background: #fbfbfc;
  }
  .attachment .bar { width: 4px; flex: none; }
  .attachment .text {
    padding: 5px 8px;
    font-size: 12px;
    line-height: 17px;
    color: #5d6169;
  }
  .block .bar  { background: #4fba85; }
  .footer .bar { background: #d9d9db; }

  /* The photo the note is about. A gradient rather than a photograph: the
     images must be reproducible from this repository by anybody, and a
     checked-in holiday snap would be a licence question for no gain. It is
     smooth on purpose — a block of noise reads as a broken image, which is the
     wrong thing to show beside a setting about compression. */
  .photo {
    margin-top: 8px;
    height: 78px;
    border-radius: 3px;
    background:
      radial-gradient(circle at 78% 22%, #ffe9a8 0 12px, rgba(255,233,168,0) 34px),
      linear-gradient(#8fc7e8 0%, #c7e2f0 46%, #86b06a 46%, #4f7f45 100%);
  }
</style>
</head>
<body>
  <div class="number">$number</div>
  <div class="post">
    <div class="message">Have a look at this!</div>
    $note
    <div class="photo"></div>
  </div>
  <script>
    // The browser reports the height it actually needed. The title is the only
    // channel --dump-dom offers back without opening a debugging port.
    document.title = String(Math.ceil(
      document.documentElement.getBoundingClientRect().height));
  </script>
</body>
</html>
HTML

	# First pass: ask how tall the page came out.
	# $sandbox is one optional flag, not a path, so word splitting is wanted.
	# shellcheck disable=SC2086
	height=$("$browser" --headless $sandbox --disable-gpu \
		--virtual-time-budget=2000 --window-size=410,1200 \
		--dump-dom "file://$work/note-$number.html" 2>/dev/null |
		sed -n 's/.*<title>\([0-9][0-9]*\)<\/title>.*/\1/p' | head -n 1)

	case "$height" in
	'' | *[!0-9]*)
		echo "could not measure variant $number; $browser reported no height" >&2
		exit 1
		;;
	esac

	# Second pass: photograph it at exactly that height.
	# shellcheck disable=SC2086
	"$browser" --headless $sandbox --disable-gpu --hide-scrollbars \
		--force-device-scale-factor=1 \
		--window-size="410,$height" \
		--screenshot="$out/note-$number.png" \
		"file://$work/note-$number.html" >/dev/null 2>&1

	if [ ! -s "$out/note-$number.png" ]; then
		echo "$browser produced no image for variant $number" >&2
		exit 1
	fi

	echo "wrote $out/note-$number.png (410x$height)"
}

render 1 '<div class="line">MediaForge: 11.9 MB → 2.3 MB (80% saved)</div>'
render 2 '<div><span class="compact">11.9 MB → 2.3 MB</span></div>'
render 3 '<div class="attachment block"><div class="bar"></div><div class="text">📦 11.9 MB → 2.3 MB · 80% saved by MediaForge</div></div>'
render 4 '<div class="attachment footer"><div class="bar"></div><div class="text">MediaForge · 11.9 MB → 2.3 MB (80% smaller)</div></div>'
render 5 ''

echo
echo "Five previews rebuilt in $out."
echo "Check them with: go test ./server -run TestThePreviewImages -v"
