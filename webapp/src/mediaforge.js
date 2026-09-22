// Copyright (c) 2026 MediaForge contributors.
// See LICENSE for license information.

// MediaForge's record button for the Mattermost message box.
//
// WHY THIS FILE HAS NO BUILD STEP AND NO DEPENDENCIES
//
// A Mattermost webapp plugin is one JavaScript file the server hands to the
// browser. The usual way to produce it is webpack + babel + TypeScript, which
// brings several hundred npm packages into a repository that is otherwise pure
// Go with a five-target cross-compile — for one button. Everything that
// toolchain would provide is available without it:
//
//   - React arrives on `window.React`, because the webapp publishes it as a
//     webpack external for exactly this purpose. `React.createElement` is JSX
//     without the compiler.
//   - The browser this plugin runs in is the one Mattermost itself targets, so
//     the language features used here need no transpiling.
//   - There are no imports, so there is nothing to bundle. This file IS the
//     bundle.
//
// The cost is that it is plain JavaScript rather than TypeScript. That is paid
// back by the contract test next to it, which runs the same file under Node
// against a stand-in for the webapp API.
//
// HOW THE RECORDING REACHES THE SERVER
//
// `registerFileUploadMethod` adds an entry to the attachment menu and hands it
// a callback. Calling that callback with a File puts the file through the
// webapp's OWN upload path — the same one the paperclip uses — so the upload
// appears in the draft as an attachment, can be removed before sending, and is
// posted with the message. The plugin never talks to the upload API itself,
// which is what keeps it out of the way of drafts, retries and error handling.
//
// The recording is converted to AAC on the SERVER (SPEC §4.3). The browser
// cannot produce `.m4a` reliably — measured on the test instance, Chromium's
// `audio/mp4` offer negotiates to `audio/mp4;codecs=opus` and yields zero
// bytes — so it uploads what it has under a name the server recognises.

(function () {
    'use strict';

    // The plugin id, which is also the prefix of its HTTP routes.
    var PLUGIN_ID = 'com.github.berzionline.mediaforge';

    // The container the browser records into, in order of preference.
    //
    // Measured on the test instance rather than assumed: headless Chromium
    // 152 reports `audio/webm;codecs=opus` supported and delivers it, while
    // `audio/mp4` reports supported, negotiates to `audio/mp4;codecs=opus`
    // and produces a zero-byte blob. Safari is the other way round. The list
    // is tried in order and the first one that the browser accepts wins; the
    // server converts whatever arrives.
    var PREFERRED_TYPES = [
        'audio/webm;codecs=opus',
        'audio/webm',
        'audio/mp4',
        'audio/ogg;codecs=opus',
    ];

    // extensionFor maps a recorder MIME type to the extension the server's
    // recogniser accepts. Anything unexpected falls back to webm, which is
    // what every browser that is not Safari produces.
    function extensionFor(mimeType) {
        var type = String(mimeType || '').split(';')[0].trim();
        if (type === 'audio/mp4') {
            return 'mp4';
        }
        if (type === 'audio/ogg') {
            return 'ogg';
        }
        return 'webm';
    }

    // pickMimeType returns the first container this browser will actually
    // record into, or '' to let the browser choose for itself.
    function pickMimeType(recorderClass) {
        if (!recorderClass || typeof recorderClass.isTypeSupported !== 'function') {
            return '';
        }
        for (var i = 0; i < PREFERRED_TYPES.length; i++) {
            if (recorderClass.isTypeSupported(PREFERRED_TYPES[i])) {
                return PREFERRED_TYPES[i];
            }
        }
        return '';
    }

    // timestampName builds the upload's file name.
    //
    // The prefix is what routes the upload into the server's voice pipeline,
    // and it is taken from what the server ANSWERED rather than written in
    // here, so the two sides cannot drift apart. The timestamp is local time
    // in a sortable shape, because it is also what the recipient sees under
    // the attachment.
    function timestampName(prefix, extension, now) {
        var d = now || new Date();
        function pad(n) {
            return (n < 10 ? '0' : '') + n;
        }
        var stamp = String(d.getFullYear()) + pad(d.getMonth() + 1) + pad(d.getDate()) +
            '-' + pad(d.getHours()) + pad(d.getMinutes()) + pad(d.getSeconds());
        return prefix + stamp + '.' + extension;
    }

    // formatDuration renders elapsed seconds as m:ss.
    function formatDuration(seconds) {
        var whole = Math.max(0, Math.floor(seconds));
        var mins = Math.floor(whole / 60);
        var secs = whole % 60;
        return mins + ':' + (secs < 10 ? '0' : '') + secs;
    }

    // fetchHealth reads the server's requirement check for the System Console.
    //
    // It exists because a missing dependency used to be silent: the server
    // switched video compression off, wrote one line to its log, and the
    // console looked exactly as it does on a healthy installation.
    //
    // Unlike fetchVoiceSettings, which fails CLOSED by hiding a button, this
    // one fails LOUD. A health display that renders nothing when it cannot
    // reach the server reproduces the very silence it was built to end, so a
    // failed request becomes a visible error row rather than an empty section.
    function fetchHealth(fetchImpl) {
        var f = fetchImpl || (typeof fetch === 'function' ? fetch : null);
        var unreachable = {
            healthy: false,
            checks: [{
                id: 'connection',
                label: 'Requirement check',
                status: 'error',
                detail: 'The requirement check could not be read from the server. ' +
                    'Reload the page; if this persists, look at the server log.',
            }],
        };
        if (!f) {
            return Promise.resolve(unreachable);
        }
        return f('/plugins/' + PLUGIN_ID + '/health', {
            credentials: 'same-origin',
            headers: {'X-Requested-With': 'XMLHttpRequest'},
        }).then(function (response) {
            if (!response.ok) {
                return unreachable;
            }
            return response.json();
        }).then(function (report) {
            if (!report || !Array.isArray(report.checks)) {
                return unreachable;
            }
            return report;
        }).catch(function () {
            return unreachable;
        });
    }

    // summariseHealth counts what the report found.
    //
    // It tolerates a malformed payload on purpose. The only producer is this
    // plugin's own server, but a browser holding a cached bundle from an older
    // version is exactly the case where the health section must not be the
    // thing that breaks.
    function summariseHealth(report) {
        var checks = (report && Array.isArray(report.checks)) ? report.checks : [];
        var errors = 0;
        var warnings = 0;
        checks.forEach(function (c) {
            if (!c) {
                return;
            }
            if (c.status === 'error') {
                errors += 1;
            } else if (c.status === 'warn') {
                warnings += 1;
            }
        });
        return {errors: errors, warnings: warnings, ok: errors === 0};
    }

    // fetchVoiceSettings asks the server whether recording is available.
    //
    // The button is drawn only if it is. A button that recorded something the
    // server would then store unconverted is worse than no button: it plays on
    // a desktop and stays silent on a phone, which is the failure SPEC §4.3
    // exists to rule out.
    //
    // The channel is passed on when it is known, because availability is now
    // per channel (SPEC §4.3). The server is the side that enforces it — this
    // request only decides whether a button is DRAWN, so that the webapp never
    // offers something the server would then decline.
    function fetchVoiceSettings(fetchImpl, channelId) {
        var f = fetchImpl || (typeof fetch === 'function' ? fetch : null);
        var closed = {enabled: false, name_prefix: '', max_seconds: 0};
        if (!f) {
            return Promise.resolve(closed);
        }
        var url = '/plugins/' + PLUGIN_ID + '/voice/settings';
        if (channelId) {
            url += '?channel_id=' + encodeURIComponent(channelId);
        }
        return f(url, {
            credentials: 'same-origin',
            headers: {'X-Requested-With': 'XMLHttpRequest'},
        }).then(function (response) {
            if (!response.ok) {
                return closed;
            }
            return response.json();
        }).catch(function () {
            // A server that cannot answer is a server that cannot convert.
            // Failing closed is the whole point of asking.
            return closed;
        });
    }

    // createAvailability answers "may this channel record" and remembers the
    // answers per channel.
    //
    // Per channel, not per session. Somebody moves between channels without
    // reloading the page, so a single cached answer would carry the button
    // from a channel where recording is offered into one where it is not —
    // and the server would then decline the upload, which is the one outcome
    // this whole mechanism exists to prevent.
    //
    // The in-flight request is shared as well as the result, so a component
    // that re-renders several times while the answer is on its way asks once.
    function createAvailability(ask) {
        var answers = {};

        return {
            forChannel: function (channelId) {
                var key = channelId || '';
                if (!answers[key]) {
                    answers[key] = Promise.resolve(ask(channelId));
                }
                return answers[key];
            },
        };
    }

    // createRecorderModel holds everything about one recording that is not a
    // React concern: the microphone, the recorder, the chunks and the clock.
    //
    // It is separated from the component so the behaviour can be tested
    // without a DOM — which is what the contract test next to this file does.
    // Every path that ends a recording releases the microphone, because a
    // browser shows a recording indicator for as long as ONE track is live,
    // and a plugin that leaves it on looks like it is listening.
    function createRecorderModel(deps) {
        var d = deps || {};
        var getUserMedia = d.getUserMedia;
        var RecorderClass = d.MediaRecorder;
        var BlobClass = d.Blob;
        var FileClass = d.File;
        var createObjectURL = d.createObjectURL;
        var revokeObjectURL = d.revokeObjectURL;
        var now = d.now || function () {
            return Date.now();
        };

        // The longest recording the SERVER will accept, as the server itself
        // reported it. Zero means "no limit known", in which case the browser
        // does not stop the recording — an invented limit would be worse than
        // none, because it would cut people off at a length nobody configured.
        var maxSeconds = Number(d.maxSeconds) || 0;

        // The Web Audio API is what measures the signal for the waveform. It
        // is optional on purpose: the recording is the feature and the
        // waveform is the reassurance, so a browser without it still records.
        var AudioContextClass = d.AudioContext ||
            (typeof window === 'undefined' ? null : (window.AudioContext || window.webkitAudioContext));

        var state = {
            phase: 'idle', // idle | recording | preview
            error: '',
            startedAt: 0,
            previewUrl: '',
            file: null,
        };

        var stream = null;
        var recorder = null;
        var chunks = [];
        var stopped = null;
        var audioContext = null;
        var analyser = null;
        var sourceNode = null;
        var sampleBuffer = null;

        // levels is the recent history the waveform is drawn from.
        //
        // It is bounded rather than complete. Ten minutes at twenty samples a
        // second is twelve thousand numbers, and the drawing only ever shows
        // the most recent stretch — so keeping them all would be a leak bought
        // for nothing.
        var levels = [];
        var MAX_LEVELS = 256;

        // startAnalysis wires the microphone into an analyser.
        //
        // The connection is what makes the waveform honest: the numbers come
        // from the stream that is actually being recorded, not from a timer or
        // a random generator. A waveform driven by Math.random() looks exactly
        // like a working one, which is why the contract test asserts on this
        // wiring rather than on the picture.
        function startAnalysis(granted) {
            levels = [];
            if (!AudioContextClass) {
                return;
            }
            try {
                audioContext = new AudioContextClass();
                analyser = audioContext.createAnalyser();
                analyser.fftSize = 2048;
                // No smoothing: the point is to show the signal moving as
                // somebody speaks, and smoothing flattens exactly that.
                analyser.smoothingTimeConstant = 0;
                sourceNode = audioContext.createMediaStreamSource(granted);
                sourceNode.connect(analyser);
                sampleBuffer = new Uint8Array(analyser.fftSize);
            } catch (err) {
                // A browser that refuses to analyse still records. The button
                // must not fail because the decoration could not be drawn.
                stopAnalysis();
            }
        }

        // stopAnalysis releases the sound card.
        //
        // An AudioContext left open keeps the browser showing that the page is
        // using audio, which is the same class of failure as leaving the
        // microphone track live — it looks like the plugin is still listening.
        function stopAnalysis() {
            if (sourceNode && typeof sourceNode.disconnect === 'function') {
                sourceNode.disconnect();
            }
            if (analyser && typeof analyser.disconnect === 'function') {
                analyser.disconnect();
            }
            if (audioContext && typeof audioContext.close === 'function') {
                try {
                    audioContext.close();
                } catch (err) {
                    // Closing twice is harmless and must not break a stop.
                }
            }
            sourceNode = null;
            analyser = null;
            audioContext = null;
            sampleBuffer = null;
        }

        function releaseMicrophone() {
            if (!stream) {
                return;
            }
            var tracks = stream.getTracks ? stream.getTracks() : [];
            for (var i = 0; i < tracks.length; i++) {
                if (typeof tracks[i].stop === 'function') {
                    tracks[i].stop();
                }
            }
            stream = null;
        }

        function dropPreview() {
            if (state.previewUrl && typeof revokeObjectURL === 'function') {
                revokeObjectURL(state.previewUrl);
            }
            state.previewUrl = '';
            state.file = null;
        }

        return {
            state: state,

            supported: function () {
                return Boolean(getUserMedia && RecorderClass);
            },

            start: function () {
                if (state.phase === 'recording') {
                    return Promise.resolve();
                }
                dropPreview();
                state.error = '';
                return getUserMedia({audio: true}).then(function (granted) {
                    stream = granted;
                    startAnalysis(granted);
                    var mimeType = pickMimeType(RecorderClass);
                    recorder = mimeType ?
                        new RecorderClass(stream, {mimeType: mimeType}) :
                        new RecorderClass(stream);
                    chunks = [];
                    recorder.ondataavailable = function (event) {
                        if (event && event.data && event.data.size) {
                            chunks.push(event.data);
                        }
                    };
                    stopped = new Promise(function (resolve) {
                        recorder.onstop = resolve;
                    });
                    // A timeslice is requested so the recorder emits data as
                    // it goes. Without it a recorder that is stopped by a page
                    // navigation hands back nothing at all.
                    recorder.start(250);
                    state.phase = 'recording';
                    state.startedAt = now();
                }).catch(function (err) {
                    releaseMicrophone();
                    stopAnalysis();
                    state.phase = 'idle';
                    // The only error a user can act on is a refused
                    // permission, so it is the only one named.
                    state.error = (err && err.name === 'NotAllowedError') ?
                        'MediaForge needs permission to use your microphone.' :
                        'No microphone was found.';
                    throw err;
                });
            },

            stop: function () {
                if (state.phase !== 'recording' || !recorder) {
                    return Promise.resolve(null);
                }
                var mimeType = recorder.mimeType;
                var prefix = d.namePrefix || '';
                recorder.stop();
                return stopped.then(function () {
                    releaseMicrophone();
                    stopAnalysis();
                    var blob = new BlobClass(chunks, {type: mimeType});
                    var name = timestampName(prefix, extensionFor(mimeType), new Date(now()));
                    state.file = new FileClass([blob], name, {type: mimeType});
                    state.previewUrl = typeof createObjectURL === 'function' ?
                        createObjectURL(blob) : '';
                    state.phase = 'preview';
                    return state.file;
                });
            },

            // discard throws the recording away, from either phase.
            //
            // It is deliberately reachable while recording as well: a user who
            // started by accident must be able to stop without producing an
            // attachment they then have to remove.
            discard: function () {
                if (recorder && state.phase === 'recording') {
                    recorder.stop();
                }
                releaseMicrophone();
                stopAnalysis();
                dropPreview();
                chunks = [];
                recorder = null;
                state.phase = 'idle';
                state.error = '';
                return Promise.resolve();
            },

            // take hands the finished recording over and returns to idle.
            //
            // The object URL is released but the File is not: the caller now
            // owns it, and the webapp's uploader reads it after this returns.
            take: function () {
                var file = state.file;
                if (state.previewUrl && typeof revokeObjectURL === 'function') {
                    revokeObjectURL(state.previewUrl);
                }
                state.previewUrl = '';
                state.file = null;
                state.phase = 'idle';
                chunks = [];
                recorder = null;
                return file;
            },

            // Put a taken file back, so a failed handover does not cost the
            // user their recording. `take()` clears the model on the assumption
            // the caller now owns the file; when the caller could not place it,
            // this restores the preview so ✓ can simply be pressed again.
            //
            // The preview URL must be recreated: take() revokes it
            // unconditionally, so reusing the old string would leave a play
            // button pointing at a dead blob — audible failure, no error.
            restore: function (file) {
                if (!file) {
                    return;
                }
                state.file = file;
                state.phase = 'preview';
                if (typeof createObjectURL === 'function') {
                    state.previewUrl = createObjectURL(file);
                }
            },

            elapsedSeconds: function () {
                if (state.phase !== 'recording') {
                    return 0;
                }
                return (now() - state.startedAt) / 1000;
            },

            // sampleLevel measures the microphone once and remembers it.
            //
            // The number is the average deviation from silence across one
            // analyser frame. Byte time-domain data is centred on 128, so
            // silence reads as 128 everywhere and a loud signal swings far
            // from it — which is exactly what a person wants to see move
            // while they speak.
            //
            // A root-mean-square would be the textbook answer and is not used:
            // it needs a square root per sample for a number that ends up
            // scaled into a few pixels of height, and the mean deviation moves
            // with the signal just as visibly.
            sampleLevel: function () {
                if (!analyser || !sampleBuffer) {
                    // No analyser means no measurement. Reporting anything
                    // else here would be inventing a waveform, which is the
                    // one thing this must not do.
                    return 0;
                }
                analyser.getByteTimeDomainData(sampleBuffer);

                var total = 0;
                for (var i = 0; i < sampleBuffer.length; i++) {
                    total += Math.abs(sampleBuffer[i] - 128);
                }
                // 128 is the largest possible deviation, so this lands in
                // 0..1 — and is clamped anyway, because a drawing that is
                // handed 1.4 would paint outside its own box.
                var level = Math.min(1, Math.max(0, (total / sampleBuffer.length) / 128));

                levels.push(level);
                if (levels.length > MAX_LEVELS) {
                    levels.shift();
                }
                return level;
            },

            // recentLevels is the history the waveform is drawn from.
            //
            // A copy, so a component cannot reach in and change the model's
            // own state by editing what it was handed.
            recentLevels: function () {
                return levels.slice();
            },

            // maxSeconds is the limit the server reported, for display.
            maxSeconds: function () {
                return maxSeconds;
            },

            // overTheLimit reports whether this recording has run past what
            // the server would accept.
            //
            // The browser stops the recording on this rather than letting the
            // upload be refused afterwards. Both are enforcement — but one of
            // them costs the sender everything they said, and the server's
            // check exists for uploads that never went through this webapp at
            // all.
            overTheLimit: function () {
                if (maxSeconds <= 0 || state.phase !== 'recording') {
                    return false;
                }
                return (now() - state.startedAt) / 1000 > maxSeconds;
            },

            // remainingSeconds is how much longer this recording may run,
            // never below zero.
            remainingSeconds: function () {
                if (maxSeconds <= 0 || state.phase !== 'recording') {
                    return 0;
                }
                var left = maxSeconds - ((now() - state.startedAt) / 1000);
                return left > 0 ? left : 0;
            },
        };
    }

    // ------------------------------------------------------------ the button

    // waveformColour picks the colour the bars are painted in.
    //
    // This is not cosmetic and it is where the first version was WRONG. A
    // canvas is not CSS: `ctx.fillStyle = 'currentColor'` is not a colour a
    // canvas understands, and instead of inheriting the text colour it falls
    // back to BLACK — measured in Chromium, which reports `#000000` after the
    // assignment. On Mattermost's dark theme that is an invisible waveform on
    // a dark background, and the code reads perfectly while producing nothing.
    // It was caught by looking at the rendered picture, which is the only way
    // it could have been caught.
    //
    // The colour is therefore read from the element's own computed style, so
    // the bars follow the theme the way the rest of the message box does,
    // with a readable blue as the fallback for anything that cannot report
    // one.
    function waveformColour(canvas) {
        var fallback = '#5d89ea';
        try {
            if (typeof getComputedStyle !== 'function') {
                return fallback;
            }
            var colour = getComputedStyle(canvas).color;
            // A transparent or unset colour would paint nothing at all, which
            // is the same failure in a different disguise.
            if (!colour || colour === 'transparent' ||
                colour.replace(/\s/g, '').indexOf('rgba(0,0,0,0)') === 0) {
                return fallback;
            }
            return colour;
        } catch (err) {
            return fallback;
        }
    }

    // The toolbar row every phase of the recorder renders into.
    //
    // This exists as one constant because getting it wrong is the defect that
    // made the whole feature unusable, twice. The component renders into the
    // message box toolbar, which is narrow and clips what does not fit: a row
    // that can wrap or grow pushes its own buttons out of sight, and the user
    // sees a half-drawn widget with no working controls and no error anywhere.
    //
    // The first version had no row style at all and put a ~300 px native
    // <audio controls> element in the preview. The second fixed the preview and
    // left the RECORDING phase untouched — which is what the operator then saw as
    // "still looks buggy": during recording the stop and discard buttons had
    // the same overflow problem the preview had just been cured of.
    //
    // `inline-flex` keeps the children on one line, `nowrap` stops the row
    // breaking in a narrow window, and `flex-shrink: 0` on the row means the
    // toolbar cannot squeeze it into an unreadable sliver.
    var RECORDER_ROW_STYLE = {
        display: 'inline-flex',
        alignItems: 'center',
        gap: '4px',
        whiteSpace: 'nowrap',
        flexShrink: 0,
        verticalAlign: 'middle',
    };

    // drawWaveform paints the recent levels onto a canvas.
    //
    // WHY THERE IS NO CHARTING LIBRARY HERE
    //
    // A waveform is the usual excuse for adding a chart package to a project.
    // This one is thirty lines: the levels are already measured, and a canvas
    // draws a bar per level. The webapp half of this plugin has no npm
    // dependencies at all and that is worth more than a library's polish for a
    // picture this small.
    //
    // It reads the canvas's own size rather than a fixed one, because the
    // message box is laid out by Mattermost's CSS and a hard-coded width would
    // be wrong on a narrow window.
    function drawWaveform(canvas, levels) {
        if (!canvas || typeof canvas.getContext !== 'function') {
            return;
        }
        var ctx = canvas.getContext('2d');
        if (!ctx) {
            return;
        }

        var width = canvas.width;
        var height = canvas.height;
        ctx.clearRect(0, 0, width, height);

        var barWidth = 2;
        var gap = 1;
        var step = barWidth + gap;
        var visible = Math.floor(width / step);
        var shown = levels.slice(-visible);

        // Drawn from the right, so the newest sound is at the leading edge and
        // the picture scrolls the way a person expects while they speak.
        ctx.fillStyle = waveformColour(canvas);
        for (var i = 0; i < shown.length; i++) {
            // A minimum of two pixels, so silence is a visible baseline rather
            // than an empty box that looks like a broken component.
            var barHeight = Math.max(2, shown[i] * height);
            var x = width - ((shown.length - i) * step);
            ctx.fillRect(x, (height - barHeight) / 2, barWidth, barHeight);
        }
    }

    function createRecorderComponent(React, model, upload, availability) {
        // availability is either a fixed answer (which is what the contract
        // tests hand in) or a live lookup with a forChannel method. The live
        // one is what production uses, because a person moves between
        // channels without reloading the page.
        var lookup = (availability && typeof availability.forChannel === 'function') ?
            availability : null;
        var fixed = lookup ? null : (availability || {enabled: true});

        return function MediaForgeRecorder(props) {
            // The channel comes from the component's own draft. The upload
            // callback registered through registerFileUploadMethod is handed
            // nothing at all (file_upload.tsx line 659), so this is the only
            // place in the webapp that knows where the message box is.
            var draft = (props && props.draft) || {};
            var channelId = draft.channelId || '';

            // the file-naming work.1: tell the sender why the send button is not available.
            //
            // A video transcode makes them wait — 2.5 min for 306 MB on two
            // cores, measured in SPEC §6.1 — and until now they were told
            // nothing at all. The button was simply unavailable, which reads as
            // a broken client rather than as work in progress.
            //
            // `draft.uploadsInProgress` is the webapp's own list of uploads it
            // is waiting for; it is handed to this component as a prop
            // (verified in the running bundle: PostEditorAction components are
            // rendered with {draft, getSelectedText, updateText}). While that
            // list is non-empty the server is holding the request open, which
            // for MediaForge means the file is being compressed.
            //
            // Deliberately NOT the other shape was offered — "send now,
            // deliver when compression finishes". That reintroduces the
            // watcher's third structural defect: a window in which a recipient
            // can fetch a file that is still being written (SPEC §2). Holding
            // the post back instead is a much larger design and its own
            // decision, not an implementation detail of a progress hint.
            var pending = (draft.uploadsInProgress || []).length;

            var phaseState = React.useState(model.state.phase);
            var phase = phaseState[0];
            var setPhase = phaseState[1];
            var elapsedState = React.useState(0);
            var elapsed = elapsedState[0];
            var setElapsed = elapsedState[1];
            var errorState = React.useState('');
            var error = errorState[0];
            var setError = errorState[1];
            // Nothing is drawn until the server has answered for THIS channel.
            // Starting from "available" would flash a button into a channel
            // that does not have the feature, and the person who pressed it in
            // that instant would be offered something the server refuses.
            var allowedState = React.useState(fixed ? fixed.enabled !== false : false);
            var allowed = allowedState[0];
            var setAllowed = allowedState[1];
            var canvasRef = React.useRef ? React.useRef(null) : {current: null};
            // Playback state for the preview. The <audio> element is created on
            // demand rather than rendered, so it cannot take up toolbar width.
            var playingState = React.useState(false);
            var playing = playingState[0];
            var setPlaying = playingState[1];
            var playerRef = React.useRef ? React.useRef(null) : {current: null};

            React.useEffect(function () {
                if (!lookup) {
                    return undefined;
                }
                var current = true;
                lookup.forChannel(channelId).then(function (settings) {
                    if (current) {
                        setAllowed(Boolean(settings && settings.enabled));
                    }
                });
                return function () {
                    // The answer for the channel that was just left must not
                    // arrive late and switch the button on in the new one.
                    current = false;
                };
            }, [channelId]);

            React.useEffect(function () {
                if (phase !== 'recording') {
                    return undefined;
                }
                // Twenty frames a second: fast enough that the waveform moves
                // with a voice, slow enough to cost nothing measurable. The
                // same timer reads the level and the clock, because two timers
                // would drift apart and show a waveform that disagrees with
                // the counter next to it.
                var timer = setInterval(function () {
                    model.sampleLevel();
                    setElapsed(model.elapsedSeconds());
                    drawWaveform(canvasRef.current, model.recentLevels());

                    // The limit is enforced HERE as well as on the server, and
                    // the two do different jobs. The server's check is what
                    // makes the rule real, because a recording can be uploaded
                    // without this webapp. This one is what keeps somebody
                    // from speaking for eleven minutes and only then being
                    // told it was too long — their words would be gone.
                    if (model.overTheLimit()) {
                        model.stop().then(function () {
                            setPhase('preview');
                        });
                    }
                }, 50);
                return function () {
                    clearInterval(timer);
                };
            }, [phase]);

            // The microphone must be released when the message box goes away,
            // otherwise the browser keeps showing its recording indicator.
            React.useEffect(function () {
                return function () {
                    model.discard();
                };
            }, []);

            function handleStart() {
                model.start().then(function () {
                    setPhase('recording');
                    setElapsed(0);
                    setError('');
                }).catch(function () {
                    setPhase('idle');
                    setError(model.state.error);
                });
            }

            function handleStop() {
                model.stop().then(function () {
                    setPhase('preview');
                });
            }

            // Playback in the preview, without a native <audio controls>
            // widget. That widget is what broke this feature: at ~300 px it
            // overflowed the message box toolbar and pushed the attach and
            // discard buttons out of sight, so a finished recording could
            // never be sent.
            //
            // The element still exists — it is simply not rendered, and is
            // driven by this one button instead. SPEC §4.3 asks that a
            // recording can be replayed before it is sent; it does not ask for
            // a browser player.
            function handlePlay() {
                var url = model.state.previewUrl;
                if (!url || typeof Audio !== 'function') {
                    return;
                }
                if (playerRef.current) {
                    playerRef.current.pause();
                    playerRef.current = null;
                    setPlaying(false);
                    return;
                }
                var player = new Audio(url);
                playerRef.current = player;
                // Reset on the way out, however playback ends, so the button
                // never claims to be playing something that has finished.
                player.onended = function () {
                    playerRef.current = null;
                    setPlaying(false);
                };
                player.onerror = player.onended;
                setPlaying(true);
                var started = player.play();
                if (started && typeof started.catch === 'function') {
                    started.catch(function () {
                        playerRef.current = null;
                        setPlaying(false);
                    });
                }
            }

            // Playback must not outlive the preview it belongs to. Without
            // this, sending or discarding leaves the recording audible with no
            // visible control to stop it.
            function stopPlayback() {
                if (playerRef.current) {
                    playerRef.current.pause();
                    playerRef.current = null;
                }
                setPlaying(false);
            }

            function handleDiscard() {
                stopPlayback();
                model.discard().then(function () {
                    setPhase('idle');
                    setElapsed(0);
                });
            }

            function handleSend() {
                stopPlayback();
                var file = model.take();
                if (!file) {
                    setPhase('idle');
                    setElapsed(0);
                    return;
                }
                if (!upload.available()) {
                    setError('This browser cannot attach the recording. ' +
                        'Please use a current version of Chrome, Edge or Firefox.');
                    return;
                }
                // The file is handed over by pasting it, which is how any file
                // reaches a draft. Mattermost signals that it took the paste by
                // calling preventDefault(); if it did not, nothing consumed the
                // recording.
                //
                // The recording is NOT discarded until the handover is known to
                // have worked. The worst outcome for someone who just spoke is
                // watching their recording vanish with no attachment and no
                // explanation — so on failure the preview stays exactly as it
                // was, with the file still in it, and can simply be pressed
                // again.
                if (!upload.send([file])) {
                    model.restore(file);
                    setError('The recording could not be attached. Please click ✓ again.');
                    return;
                }
                setPhase('idle');
                setElapsed(0);
            }

            function button(props, label) {
                return React.createElement('button', Object.assign({
                    type: 'button',
                    className: 'style--none AdvancedTextEditor__action-button',
                }, props), label);
            }

            // A channel the server did not open up draws NOTHING. A button
            // that offers a recording the server would then decline to
            // convert is worse than no button: it makes a promise and breaks
            // it, and the person only finds out after they have spoken.
            //
            // This is checked after the hooks rather than before them,
            // because React requires the same hooks to run on every render of
            // a component and this value changes as the person moves between
            // channels.
            //
            // The waiting hint comes BEFORE those gates, but AFTER a recording
            // in progress. Two reasons, and both are load-bearing:
            //
            //  - Before the voice gates: compression applies to every image and
            //    video upload whether or not voice messages are switched on, so
            //    a sender waiting for a video must be told why even on a server
            //    with no record button and in a browser too old to record.
            //  - After a live recording: replacing an active recorder with
            //    "Compressing…" would take away the stop button mid-sentence.
            //    An upload finishing while someone speaks is ordinary, and the
            //    recording must win.
            if (pending > 0 && phase === 'idle') {
                return React.createElement('span', {
                    className: 'MediaForgeRecorder MediaForgeRecorder--waiting',
                    style: RECORDER_ROW_STYLE,
                    // Announced once, not on every re-render of the count.
                    'aria-live': 'polite',
                },
                    React.createElement('span', {
                        // A plain word rather than a spinner: the plugin ships
                        // no stylesheet, so an animation would need keyframes
                        // that cannot be delivered from here.
                        style: {opacity: 0.75, fontSize: '12px'},
                    }, pending > 1 ?
                        'Compressing ' + pending + ' files…' :
                        'Compressing…'));
            }

            if (!allowed) {
                return null;
            }

            if (phase === 'recording') {
                // The waveform is the point of this slice's visible half: it
                // is how a person knows the microphone really hears them,
                // which a bare stopwatch cannot tell them — a counter ticks
                // along just as happily on a muted microphone.
                //
                // The remaining time is only shown once it is worth worrying
                // about. Displaying "0:03 / -9:57" from the first second made
                // the row needlessly wide in a toolbar that has no width to
                // spare, and told the user nothing they needed: what matters is
                // the warning as the limit approaches, not arithmetic from the
                // start. Under a minute left, it appears.
                var left = model.remainingSeconds();
                var remaining = (model.maxSeconds() > 0 && left <= 60) ?
                    ' / -' + formatDuration(left) : '';

                return React.createElement('span', {
                    className: 'MediaForgeRecorder',
                    style: RECORDER_ROW_STYLE,
                },
                    React.createElement('canvas', {
                        className: 'MediaForgeRecorder__waveform',
                        ref: canvasRef,
                        width: 96,
                        height: 24,
                        // Fixed box: without it the canvas is laid out at its
                        // attribute size but may still be stretched by the
                        // flex row, which is what made the waveform look
                        // smeared rather than drawn.
                        style: {width: '96px', height: '24px', flexShrink: 0},
                        // The picture carries no information a screen reader
                        // can use; the time next to it does, and is announced.
                        'aria-hidden': 'true',
                    }),
                    React.createElement('span', {
                        className: 'MediaForgeRecorder__time',
                        'aria-live': 'polite',
                        // A monospaced, reserved width stops the row jittering
                        // sideways every time the seconds tick over.
                        style: {
                            fontVariantNumeric: 'tabular-nums',
                            minWidth: '2.6em',
                            textAlign: 'right',
                        },
                    }, formatDuration(elapsed) + remaining),
                    button({
                        onClick: handleStop,
                        'aria-label': 'Stop recording',
                        title: 'Stop recording',
                    }, '⏹'),
                    button({
                        onClick: handleDiscard,
                        'aria-label': 'Discard recording',
                        title: 'Discard recording',
                    }, '🗑'));
            }

            if (phase === 'preview') {
                // Order matters, and it is the fix for the defect the cutover
                // found: the ✓ and 🗑 buttons come FIRST.
                //
                // This component renders into the message box TOOLBAR, which is
                // narrow. The first version put a native `<audio controls>`
                // element before the buttons — that element is ~300 px wide and
                // does not shrink, so it overflowed the toolbar and pushed both
                // buttons out of sight. The recording could be made and played
                // but never attached, because nothing could reach handleSend.
                // Nothing threw, so the console stayed silent and every test
                // that only checked "was upload.send called" stayed green.
                //
                // The playback control is therefore a small button that toggles
                // an off-screen <audio> element, not a full player widget: the
                // requirement from SPEC §4.3 is that the recording can be
                // replayed before sending, not that a browser player is shown.
                return React.createElement('span', {
                    className: 'MediaForgeRecorder',
                    style: RECORDER_ROW_STYLE,
                },
                    React.createElement('canvas', {
                        className: 'MediaForgeRecorder__waveform',
                        ref: function (node) {
                            canvasRef.current = node;
                            drawWaveform(node, model.recentLevels());
                        },
                        width: 96,
                        height: 24,
                        style: {width: '96px', height: '24px', flexShrink: 0},
                        'aria-hidden': 'true',
                    }),
                    React.createElement('span', {
                        className: 'MediaForgeRecorder__time',
                        style: {
                            fontVariantNumeric: 'tabular-nums',
                            minWidth: '2.6em',
                            textAlign: 'right',
                        },
                    }, formatDuration(elapsed)),
                    button({
                        onClick: handlePlay,
                        'aria-label': playing ? 'Stop playback' : 'Play the recording',
                        title: playing ? 'Stop playback' : 'Play the recording',
                    }, playing ? '⏸' : '▶'),
                    button({
                        onClick: handleSend,
                        'aria-label': 'Attach this recording',
                        title: 'Attach this recording',
                    }, '✓'),
                    button({
                        onClick: handleDiscard,
                        'aria-label': 'Discard recording',
                        title: 'Discard recording',
                    }, '🗑'));
            }

            if (phase === 'idle' && error) {
                // An error is shown next to the button rather than in place of
                // it, so the user can read what happened and try again.
                return React.createElement('span', {className: 'MediaForgeRecorder'},
                    button({
                        onClick: handleStart,
                        'aria-label': 'Record a voice message',
                        title: error,
                    }, '🎙'),
                    React.createElement('span', {
                        className: 'MediaForgeRecorder__error',
                        role: 'alert',
                    }, error));
            }

            return React.createElement('span', {className: 'MediaForgeRecorder'},
                button({
                    onClick: handleStart,
                    'aria-label': 'Record a voice message',
                    title: 'Record a voice message',
                }, '🎙'));
        };
    }

    // ------------------------------------------------------------- the plugin

    // createHealthComponent renders the requirement check at the top of the
    // MediaForge settings page.
    //
    // It is a custom setting rather than a line of help text because the thing
    // it reports is MEASURED on the server at the moment the page is opened —
    // which ffmpeg is installed, which encoders that build has, how much room
    // the spool directory has left. None of that can be expressed in the
    // static settings schema, and that gap is why a missing ffmpeg was
    // invisible until somebody uploaded a video.
    function createHealthComponent(React, fetchImpl) {
        var colours = {
            ok: '#06d6a0',
            warn: '#ffbc1f',
            error: '#d24b4e',
        };
        var symbols = {ok: '\u2713', warn: '!', error: '\u2715'};

        return function HealthSection() {
            var state = React.useState(null);
            var report = state[0];
            var setReport = state[1];

            React.useEffect(function () {
                var cancelled = false;
                fetchHealth(fetchImpl).then(function (r) {
                    if (!cancelled) {
                        setReport(r);
                    }
                });
                return function () {
                    cancelled = true;
                };
            }, []);

            if (!report) {
                return React.createElement('div', {style: {padding: '8px 0', opacity: 0.7}},
                    'Checking the server\u2019s requirements\u2026');
            }

            var summary = summariseHealth(report);
            var heading = summary.ok ?
                'All requirements are met.' :
                summary.errors + ' requirement' + (summary.errors === 1 ? '' : 's') +
                    ' need attention before MediaForge works fully.';

            var rows = (report.checks || []).map(function (c, i) {
                var colour = colours[c.status] || colours.warn;
                return React.createElement('li', {
                    key: c.id || i,
                    style: {display: 'flex', gap: '8px', padding: '4px 0', listStyle: 'none'},
                }, [
                    React.createElement('span', {
                        key: 'sym',
                        'aria-hidden': 'true',
                        style: {color: colour, fontWeight: 'bold', width: '1em', flexShrink: 0},
                    }, symbols[c.status] || '?'),
                    React.createElement('span', {key: 'txt'}, [
                        React.createElement('strong', {key: 'label'}, c.label || c.id),
                        // The status word is spelled out as well as coloured:
                        // a red dot alone is not readable to everyone, and the
                        // whole point of this section is that it gets read.
                        React.createElement('span', {key: 'sep'}, ' \u2014 '),
                        React.createElement('span', {key: 'detail'}, c.detail || ''),
                    ]),
                ]);
            });

            return React.createElement('div', {
                style: {
                    border: '1px solid rgba(63,67,80,0.16)',
                    borderLeft: '4px solid ' + (summary.ok ? colours.ok : colours.error),
                    borderRadius: '4px',
                    padding: '12px 16px',
                    marginBottom: '16px',
                },
            }, [
                React.createElement('div', {
                    key: 'head',
                    style: {fontWeight: 600, marginBottom: '8px'},
                }, heading),
                React.createElement('ul', {key: 'list', style: {margin: 0, padding: 0}}, rows),
            ]);
        };
    }

    function MediaForgePlugin() {
        this.registeredId = null;
    }

    MediaForgePlugin.prototype.initialize = function (registry, store, deps) {
        var d = deps || {};
        var w = d.window || (typeof window === 'undefined' ? {} : window);
        var React = d.React || w.React;
        var self = this;

        // The health section is registered before anything else and
        // independently of the voice feature, because it is the thing that
        // explains an installation where the other features are missing. If it
        // were registered inside the voice path below, a server without ffmpeg
        // would fail that path and show no health section — precisely when the
        // administrator needs it most.
        if (React && registry && typeof registry.registerAdminConsoleCustomSetting === 'function') {
            registry.registerAdminConsoleCustomSetting(
                'HealthReport',
                createHealthComponent(React, d.fetch),
            );
        }

        // The first question is asked without a channel, because at load time
        // there is none. It decides whether anything is registered at all: a
        // server with the feature switched off, or without ffmpeg, sends no
        // name prefix, and the message box stays exactly as it is without the
        // plugin.
        //
        // The prefix rather than `enabled` is what is read here, and that is
        // deliberate. With a channel list configured the server answers
        // "enabled: false" to a channel-less question — correctly, because it
        // cannot know where the caller is — and reading that as "register
        // nothing" would remove the button from the opened-up channels too.
        // The per-channel answer is asked again by the component, for the
        // channel it is actually sitting in.
        return fetchVoiceSettings(d.fetch).then(function (settings) {
            if (!settings || !React || !settings.name_prefix) {
                return null;
            }

            var model = createRecorderModel({
                getUserMedia: d.getUserMedia || (w.navigator && w.navigator.mediaDevices &&
                    w.navigator.mediaDevices.getUserMedia.bind(w.navigator.mediaDevices)),
                MediaRecorder: d.MediaRecorder || w.MediaRecorder,
                Blob: d.Blob || w.Blob,
                File: d.File || w.File,
                createObjectURL: d.createObjectURL ||
                    (w.URL && w.URL.createObjectURL.bind(w.URL)),
                revokeObjectURL: d.revokeObjectURL ||
                    (w.URL && w.URL.revokeObjectURL.bind(w.URL)),
                AudioContext: d.AudioContext || w.AudioContext || w.webkitAudioContext,
                namePrefix: settings.name_prefix,
                // The limit comes from the server, never from a number written
                // in here: two copies of one rule drift apart, and the symptom
                // would be a browser stopping at ten minutes while the server
                // refuses at five.
                maxSeconds: settings.max_seconds,
                now: d.now,
            });

            if (!model.supported()) {
                // An old browser gets no button rather than one that fails
                // when it is pressed.
                return null;
            }

            // How a recording gets into the draft.
            //
            // Mattermost hands a plugin the webapp's own upload function in
            // exactly one place: the callback of a `registerFileUploadMethod`
            // menu entry. Nothing hands it to the message box toolbar, where
            // the record button sits — which made the first version unusable.
            // The user pressed the mic, spoke, pressed ✓, and got an error
            // telling them to go and use the attachment menu first. Two
            // entrances for one feature, and the obvious one was a dead end.
            //
            // a deployment decision: the mic button in the toolbar stays and is the
            // only entrance. The menu entry goes.
            //
            // So the recording is handed over the way a user hands over any
            // file — by pasting it. `FileUpload.pasteUpload` listens on
            // `document`, filters clipboard items for `kind === 'file'`, and
            // calls `checkPluginHooksAndUploadFiles` — the SAME function the
            // menu callback would have supplied. The file therefore takes the
            // identical path: plugin hooks run, it lands in the draft as a
            // removable attachment, and it posts with the message.
            //
            // Verified in Chromium rather than assumed, because a synthetic
            // ClipboardEvent is exactly the kind of thing a browser may refuse:
            //   - `new ClipboardEvent('paste', {clipboardData: dt})` keeps the
            //     DataTransfer intact and the listener sees `file:audio/webm`
            //   - `dt.types` is `["Files"]` with no `text/plain`, so the
            //     text-paste guard does not divert it
            //   - `getAsFile().name` survives, so Mattermost keeps our filename
            //     instead of renaming it "Image Pasted at ..."
            //   - the event bubbles from inside the message box up to document
            //
            // The recording is never lost silently: if the paste does not take,
            // the file is put back and the error says so.
            var upload = {
                available: function () {
                    return typeof w.DataTransfer === 'function' &&
                        typeof w.ClipboardEvent === 'function';
                },
                send: function (files) {
                    var dt = new w.DataTransfer();
                    files.forEach(function (file) {
                        dt.items.add(file);
                    });
                    // Dispatch from the focused element inside the message box
                    // when there is one, so `containsEventTarget` recognises the
                    // paste as belonging to this textbox; fall back to the
                    // textbox itself, then to the document.
                    var target = w.document.getElementById('post_textbox') ||
                        w.document.activeElement || w.document.body;
                    var event = new w.ClipboardEvent('paste', {
                        clipboardData: dt,
                        bubbles: true,
                        cancelable: true,
                    });
                    target.dispatchEvent(event);
                    // `pasteUpload` calls preventDefault() when it has taken
                    // the files. If it did not, nothing consumed them.
                    return event.defaultPrevented;
                },
            };
            // Availability is asked per channel, live. The component drives it
            // from its own PostDraft, because that is the only place in the
            // webapp that knows which channel the message box belongs to.
            var availability = createAvailability(function (channelId) {
                return fetchVoiceSettings(d.fetch, channelId);
            });
            var Recorder = createRecorderComponent(React, model, upload, availability);

            // Exposed for the contract test, which needs to drive the paste
            // handover the way the send button does. There is no other way in:
            // the wrapper is a closure local, and the alternative would be to
            // test a copy of it rather than the object the button actually uses.
            if (typeof module !== 'undefined' && module.exports) {
                module.exports.__lastUpload = upload;
            }

            // No attachment-menu entry any more. It existed only to hand over
            // the upload callback, and as a user-facing control it was worse
            // than useless: clicking it appeared to do nothing, because it
            // started a recording whose display was clipped out of the toolbar.
            // The mic button in the message box is the single entrance.
            registry.registerPostEditorActionComponent(Recorder);
            return Recorder;
        });
    };

    MediaForgePlugin.prototype.uninitialize = function () {
        this.registeredId = null;
    };

    // Exported for the contract test, which loads this file under Node.
    if (typeof module !== 'undefined' && module.exports) {
        module.exports = {
            MediaForgePlugin: MediaForgePlugin,
            createRecorderModel: createRecorderModel,
            createRecorderComponent: createRecorderComponent,
            fetchVoiceSettings: fetchVoiceSettings,
            fetchHealth: fetchHealth,
            summariseHealth: summariseHealth,
            createAvailability: createAvailability,
            drawWaveform: drawWaveform,
            timestampName: timestampName,
            formatDuration: formatDuration,
            extensionFor: extensionFor,
            pickMimeType: pickMimeType,
            PLUGIN_ID: PLUGIN_ID,
            PREFERRED_TYPES: PREFERRED_TYPES,
        };
    }

    if (typeof window !== 'undefined' && typeof window.registerPlugin === 'function') {
        window.registerPlugin(PLUGIN_ID, new MediaForgePlugin());
    }
}());
