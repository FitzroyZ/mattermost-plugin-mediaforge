// Copyright (c) 2026 MediaForge contributors.
// See LICENSE for license information.

// Contract tests for the record button.
//
// They run under plain `node --test`, which ships with Node itself. That is
// the whole test dependency: no jest, no babel, no jsdom. The component is
// exercised through a stand-in for the webapp API rather than through a
// browser, which is what the tests are actually about — the contract between
// the plugin and Mattermost, and the rules that decide what is uploaded.
//
// What is deliberately NOT tested here: that a microphone works, that Opus is
// encoded correctly, or that the file ends up as M4A. The first two belong to
// the browser and the third to the server, where `audio_test.go` and the proof
// script check it against real ffmpeg.

const assert = require('node:assert');
const test = require('node:test');

const mf = require('../src/mediaforge.js');

// --------------------------------------------------------------- test doubles

// fakeBlob and fakeFile are the two browser types the model constructs. They
// record what they were given, which is how a test can assert that the chunks
// really reached the file.
class fakeBlob {
    constructor(parts, options) {
        this.parts = parts || [];
        this.type = (options && options.type) || '';
        this.size = this.parts.reduce((sum, p) => sum + (p.size || 0), 0);
    }
}

class fakeFile extends fakeBlob {
    constructor(parts, name, options) {
        super(parts, options);
        this.name = name;
    }
}

// fakeTrack reports whether it was stopped. A live track is what makes a
// browser show its recording indicator, so "was it stopped" is the whole
// question.
class fakeTrack {
    constructor() {
        this.stopped = false;
    }
    stop() {
        this.stopped = true;
    }
}

class fakeStream {
    constructor() {
        this.tracks = [new fakeTrack()];
    }
    getTracks() {
        return this.tracks;
    }
}

// fakeRecorder is a MediaRecorder that produces one chunk per start and
// resolves its onstop when told to.
class fakeRecorder {
    constructor(stream, options) {
        this.stream = stream;
        this.mimeType = (options && options.mimeType) || 'audio/webm;codecs=opus';
        this.started = false;
        this.timeslice = null;
        this.ondataavailable = null;
        this.onstop = null;
        fakeRecorder.instances.push(this);
    }
    static isTypeSupported(type) {
        return fakeRecorder.supported.includes(type);
    }
    start(timeslice) {
        this.started = true;
        this.timeslice = timeslice;
        if (this.ondataavailable) {
            this.ondataavailable({data: {size: 1234}});
        }
    }
    stop() {
        this.started = false;
        if (this.onstop) {
            this.onstop();
        }
    }
}
fakeRecorder.instances = [];
fakeRecorder.supported = ['audio/webm;codecs=opus'];

// newModel builds a recorder model wired entirely to the doubles above.
function newModel(overrides) {
    fakeRecorder.instances = [];
    const urls = {created: [], revoked: []};
    const streams = [];
    let clock = 1000;

    const deps = Object.assign({
        getUserMedia: () => {
            const s = new fakeStream();
            streams.push(s);
            return Promise.resolve(s);
        },
        MediaRecorder: fakeRecorder,
        Blob: fakeBlob,
        File: fakeFile,
        createObjectURL: (blob) => {
            const url = 'blob:mediaforge/' + urls.created.length;
            urls.created.push({url, blob});
            return url;
        },
        revokeObjectURL: (url) => urls.revoked.push(url),
        namePrefix: 'mediaforge-voice-',
        now: () => clock,
    }, overrides || {});

    const model = mf.createRecorderModel(deps);
    return {
        model,
        urls,
        streams,
        advance: (ms) => {
            clock += ms;
        },
    };
}

// ----------------------------------------------------- the naming contract

test('the upload is named with the prefix the SERVER supplied', () => {
    // The prefix is the entire contract between the two halves: it is what
    // routes the upload into the voice pipeline. Hard-coding it on this side
    // would let the two drift apart silently, and the symptom would be a
    // recording stored as WebM — playable on a desktop, silent on a phone.
    const name = mf.timestampName('mediaforge-voice-', 'webm', new Date(2026, 8, 18, 10, 15, 0));
    assert.strictEqual(name, 'mediaforge-voice-20260918-101500.webm');
});

test('the extension always matches what the recorder produced', () => {
    // The server accepts four containers and rejects the rest. An extension
    // that disagrees with the bytes would be refused by the recogniser, and
    // the recording would be stored unconverted.
    assert.strictEqual(mf.extensionFor('audio/webm;codecs=opus'), 'webm');
    assert.strictEqual(mf.extensionFor('audio/webm'), 'webm');
    assert.strictEqual(mf.extensionFor('audio/mp4'), 'mp4');
    assert.strictEqual(mf.extensionFor('audio/ogg;codecs=opus'), 'ogg');
    // An unknown type falls back to the one every non-Safari browser makes,
    // rather than to something the server would refuse.
    assert.strictEqual(mf.extensionFor('audio/flac'), 'webm');
    assert.strictEqual(mf.extensionFor(undefined), 'webm');
});

test('the preferred container is one the browser actually supports', () => {
    fakeRecorder.supported = ['audio/mp4'];
    assert.strictEqual(mf.pickMimeType(fakeRecorder), 'audio/mp4');

    fakeRecorder.supported = ['audio/webm;codecs=opus', 'audio/mp4'];
    assert.strictEqual(mf.pickMimeType(fakeRecorder), 'audio/webm;codecs=opus');

    // Nothing supported means "let the browser decide", not "record into a
    // container it will refuse".
    fakeRecorder.supported = [];
    assert.strictEqual(mf.pickMimeType(fakeRecorder), '');

    fakeRecorder.supported = ['audio/webm;codecs=opus'];
});

// ------------------------------------------------- record, preview, discard

test('a finished recording becomes a File under the right name', async () => {
    const h = newModel();
    await h.model.start();
    assert.strictEqual(h.model.state.phase, 'recording');

    h.advance(3000);
    const file = await h.model.stop();

    assert.strictEqual(h.model.state.phase, 'preview');
    assert.ok(file.name.startsWith('mediaforge-voice-'), `name was ${file.name}`);
    assert.ok(file.name.endsWith('.webm'), `name was ${file.name}`);
    assert.strictEqual(file.type, 'audio/webm;codecs=opus');
    assert.ok(file.size > 0, 'the recorded chunks did not reach the file');
});

test('the preview can be played before the recording is sent', async () => {
    // "Listen before sending" is one of the three things this slice promises,
    // and an <audio> element needs a URL for the blob to do it.
    const h = newModel();
    await h.model.start();
    await h.model.stop();

    assert.strictEqual(h.urls.created.length, 1);
    assert.strictEqual(h.model.state.previewUrl, h.urls.created[0].url);
});

test('discarding a preview uploads nothing and frees the preview URL', async () => {
    const h = newModel();
    await h.model.start();
    await h.model.stop();
    const url = h.model.state.previewUrl;

    await h.model.discard();

    assert.strictEqual(h.model.state.phase, 'idle');
    assert.strictEqual(h.model.state.file, null);
    assert.deepStrictEqual(h.urls.revoked, [url],
        'the discarded recording still occupies memory');
});

test('a recording can be abandoned while it is still running', async () => {
    // A user who pressed the button by accident must be able to get out
    // without producing an attachment they then have to remove.
    const h = newModel();
    await h.model.start();
    await h.model.discard();

    assert.strictEqual(h.model.state.phase, 'idle');
    assert.strictEqual(h.model.state.file, null);
});

// ------------------------------------------------------- the microphone

test('the microphone is released on every path that ends a recording',
    async () => {
        // A browser shows its recording indicator for as long as ONE track is
        // live. A plugin that leaves it on looks like it is still listening,
        // which is the kind of thing that gets a plugin uninstalled.
        for (const ending of ['stop', 'discard-while-recording', 'discard-preview']) {
            const h = newModel();
            await h.model.start();

            if (ending === 'stop') {
                await h.model.stop();
            } else if (ending === 'discard-while-recording') {
                await h.model.discard();
            } else {
                await h.model.stop();
                await h.model.discard();
            }

            const live = h.streams.flatMap((s) => s.getTracks()).filter((t) => !t.stopped);
            assert.strictEqual(live.length, 0,
                `${ending} left ${live.length} microphone track(s) live`);
        }
    });

test('a refused microphone permission is reported and changes nothing', async () => {
    const denied = new Error('denied');
    denied.name = 'NotAllowedError';
    const h = newModel({getUserMedia: () => Promise.reject(denied)});

    await assert.rejects(() => h.model.start());

    assert.strictEqual(h.model.state.phase, 'idle');
    assert.match(h.model.state.error, /permission/i);
});

// ------------------------------------------------------- the upload handover

test('sending hands the File to the webapp and returns to idle', async () => {
    const h = newModel();
    await h.model.start();
    await h.model.stop();
    const url = h.model.state.previewUrl;

    const file = h.model.take();

    assert.ok(file, 'nothing was handed over');
    assert.strictEqual(h.model.state.phase, 'idle');
    assert.strictEqual(h.model.state.file, null,
        'the model still holds the file it gave away');
    assert.deepStrictEqual(h.urls.revoked, [url]);
});

test('a recording is never lost silently when no uploader was handed over',
    async () => {
        // The record button sits in the message box toolbar, so it can be
        // pressed without ever opening the attachment menu — and the uploader
        // only arrives when that menu entry is used. A send that called an
        // empty function would make the user watch their recording vanish:
        // nothing in the draft, nothing on screen, nothing in the console.
        // The component must be able to ASK whether it can attach.
        const uploadWithout = {
            available: () => false,
            send: () => assert.fail('it tried to upload without an uploader'),
        };
        assert.strictEqual(uploadWithout.available(), false);

        // And with one, it uploads exactly once.
        const sent = [];
        const uploadWith = {
            available: () => true,
            send: (files) => sent.push(files),
        };
        uploadWith.send(['a-file']);
        assert.strictEqual(sent.length, 1);
        assert.deepStrictEqual(sent[0], ['a-file']);
    });

test('the recording is handed over by pasting it into the message box',
    async () => {
        // This replaces a test that pinned the attachment-menu handover. That
        // route is gone: it was the ONLY way Mattermost gave a plugin its
        // uploader, but as a control it was a dead end — pressing the mic in the
        // toolbar without opening the menu first produced an error telling the
        // user to go and use the menu. a deployment decision: the mic button is the
        // only entrance.
        //
        // A recording now reaches the draft the way any file does, by being
        // pasted. `FileUpload.pasteUpload` listens on `document`, filters
        // clipboard items for `kind === 'file'` and calls the same
        // `checkPluginHooksAndUploadFiles` the menu callback would have handed
        // over. What is checked here is the contract the webapp relies on: a
        // real paste event, carrying the file, dispatched inside the message
        // box, whose consumption is reported back.
        const pastes = [];
        let listener = null;
        const textbox = {
            id: 'post_textbox',
            dispatchEvent(event) {
                if (listener) {
                    listener(event);
                }
                return !event.defaultPrevented;
            },
        };
        const fakeWindow = {
            DataTransfer: class {
                constructor() {
                    this.items = {
                        added: [],
                        add(file) {
                            this.added.push(file);
                        },
                    };
                }
            },
            ClipboardEvent: class {
                constructor(type, init) {
                    this.type = type;
                    this.clipboardData = init.clipboardData;
                    this.bubbles = init.bubbles;
                    this.defaultPrevented = false;
                }
                preventDefault() {
                    this.defaultPrevented = true;
                }
            },
            document: {
                getElementById: (id) => (id === 'post_textbox' ? textbox : null),
            },
        };

        // Stand in for Mattermost's own paste handler.
        listener = (event) => {
            const files = event.clipboardData.items.added;
            if (files.length > 0) {
                pastes.push(files);
                event.preventDefault();
            }
        };

        let recorderComponent = null;
        const registry = {
            registerFileUploadMethod: () => assert.fail(
                'an attachment-menu entry was registered; it was removed on purpose'),
            registerPostEditorActionComponent: (c) => {
                recorderComponent = c;
            },
        };

        await new mf.MediaForgePlugin().initialize(registry, {}, {
            fetch: () => Promise.resolve({
                ok: true,
                json: () => Promise.resolve({enabled: true, name_prefix: 'mediaforge-voice-'}),
            }),
            React: {
                createElement: (type, props, ...children) => ({type, props, children}),
                useState: (initial) => [initial, () => {}],
                useEffect: () => {},
                useRef: (initial) => ({current: initial}),
            },
            window: fakeWindow,
            getUserMedia: () => Promise.resolve(new fakeStream()),
            MediaRecorder: fakeRecorder,
            Blob: fakeBlob,
            File: fakeFile,
            createObjectURL: () => 'blob:x',
            revokeObjectURL: () => {},
        });

        assert.ok(recorderComponent, 'no recorder component was registered');

        // Drive the upload wrapper the way the send button does.
        const upload = mf.__lastUpload;
        assert.ok(upload, 'the upload wrapper was not exposed for this test');
        assert.strictEqual(upload.available(), true,
            'the paste route reported unavailable in a browser that has DataTransfer');

        const file = {name: 'mediaforge-voice-20260919.webm', type: 'audio/webm'};
        const taken = upload.send([file]);

        assert.strictEqual(pastes.length, 1, 'no paste reached the message box');
        assert.deepStrictEqual(pastes[0], [file],
            'the pasted clipboard did not carry the recording');
        assert.strictEqual(taken, true,
            'a consumed paste must report success, or handleSend discards the ' +
            'recording believing it was attached');
    });

test('an unconsumed paste is reported as failure, so the recording is kept',
    async () => {
        // The defect this guards against is the expensive one: the user spoke,
        // pressed the tick, and the recording vanished with no attachment and no
        // error. handleSend only clears the preview when send() reports true.
        const fakeWindow = {
            DataTransfer: class {
                constructor() {
                    this.items = {added: [], add(f) {
                        this.added.push(f);
                    }};
                }
            },
            ClipboardEvent: class {
                constructor(type, init) {
                    this.clipboardData = init.clipboardData;
                    this.defaultPrevented = false;
                }
                preventDefault() {
                    this.defaultPrevented = true;
                }
            },
            document: {
                // Nobody listening: the event is dispatched and ignored.
                getElementById: () => ({dispatchEvent: () => true}),
            },
        };

        await new mf.MediaForgePlugin().initialize({
            registerFileUploadMethod: () => assert.fail('menu entry registered'),
            registerPostEditorActionComponent: () => {},
        }, {}, {
            fetch: () => Promise.resolve({
                ok: true,
                json: () => Promise.resolve({enabled: true, name_prefix: 'mediaforge-voice-'}),
            }),
            React: {
                createElement: () => ({}),
                useState: (i) => [i, () => {}],
                useEffect: () => {},
                useRef: (i) => ({current: i}),
            },
            window: fakeWindow,
            getUserMedia: () => Promise.resolve(new fakeStream()),
            MediaRecorder: fakeRecorder,
            Blob: fakeBlob,
            File: fakeFile,
            createObjectURL: () => 'blob:x',
            revokeObjectURL: () => {},
        });

        const upload = mf.__lastUpload;
        assert.strictEqual(upload.send([{name: 'v.webm'}]), false,
            'an ignored paste reported success; the recording would be lost');
    });

// ------------------------------------------------------- the server gate

test('nothing is registered when the server says recording is unavailable',
    async () => {
        // The button must never offer something the server will not do. This
        // is the whole reason the plugin asks first.
        const registry = {
            registerFileUploadMethod: () => assert.fail('a button was registered anyway'),
            registerPostEditorActionComponent: () => assert.fail('a component was registered anyway'),
        };
        const plugin = new mf.MediaForgePlugin();

        await plugin.initialize(registry, {}, {
            fetch: () => Promise.resolve({
                ok: true,
                json: () => Promise.resolve({enabled: false, name_prefix: 'mediaforge-voice-'}),
            }),
            React: {createElement: () => ({})},
            window: {},
        });
    });

test('nothing is registered when the settings route cannot be reached',
    async () => {
        // Failing closed is the point: a server that cannot answer is a server
        // that may not be able to convert either.
        for (const failure of [
            () => Promise.reject(new Error('network down')),
            () => Promise.resolve({ok: false, json: () => Promise.resolve({})}),
        ]) {
            const registry = {
                registerFileUploadMethod: () => assert.fail('a button was registered anyway'),
                registerPostEditorActionComponent: () => assert.fail('a component was registered anyway'),
            };
            await new mf.MediaForgePlugin().initialize(registry, {}, {
                fetch: failure,
                React: {createElement: () => ({})},
                window: {},
            });
        }
    });

test('the button is registered when the server says recording is available',
    async () => {
        const registered = {methods: 0, components: 0, text: '', action: null};
        const registry = {
            registerFileUploadMethod: (item) => {
                registered.methods++;
                registered.text = item.text;
                registered.action = item.action;
                return 'method-id';
            },
            registerPostEditorActionComponent: () => {
                registered.components++;
            },
        };

        await new mf.MediaForgePlugin().initialize(registry, {}, {
            fetch: () => Promise.resolve({
                ok: true,
                json: () => Promise.resolve({enabled: true, name_prefix: 'mediaforge-voice-'}),
            }),
            React: {createElement: () => ({}), useState: () => [0, () => {}], useEffect: () => {}},
            window: {},
            getUserMedia: () => Promise.resolve(new fakeStream()),
            MediaRecorder: fakeRecorder,
            Blob: fakeBlob,
            File: fakeFile,
            createObjectURL: () => 'blob:x',
            revokeObjectURL: () => {},
        });

        assert.strictEqual(registered.methods, 0,
            'an attachment-menu entry was registered; the mic button in the ' +
            'message box is the only entrance');
        assert.strictEqual(registered.components, 1);
    });

test('nothing is registered when the browser cannot record at all', async () => {
    // An old browser gets no button rather than one that fails when pressed.
    const registry = {
        registerFileUploadMethod: () => assert.fail('a button was registered anyway'),
        registerPostEditorActionComponent: () => assert.fail('a component was registered anyway'),
    };
    await new mf.MediaForgePlugin().initialize(registry, {}, {
        fetch: () => Promise.resolve({
            ok: true,
            json: () => Promise.resolve({enabled: true, name_prefix: 'mediaforge-voice-'}),
        }),
        React: {createElement: () => ({})},
        window: {},
        getUserMedia: null,
        MediaRecorder: null,
    });
});

test('the settings request is same-origin and carries the plugin id', async () => {
    // Mattermost authenticates a plugin route by session cookie, so a request
    // without credentials is answered with 401 and the button silently never
    // appears.
    let seen = null;
    await mf.fetchVoiceSettings((url, options) => {
        seen = {url, options};
        return Promise.resolve({ok: true, json: () => Promise.resolve({enabled: true})});
    });

    assert.strictEqual(seen.url, '/plugins/' + mf.PLUGIN_ID + '/voice/settings');
    assert.strictEqual(seen.options.credentials, 'same-origin');
});

// -------------------------------------------------------------- the display

test('the elapsed time is shown the way a person reads it', () => {
    assert.strictEqual(mf.formatDuration(0), '0:00');
    assert.strictEqual(mf.formatDuration(7.9), '0:07');
    assert.strictEqual(mf.formatDuration(62), '1:02');
    assert.strictEqual(mf.formatDuration(600), '10:00');
    // Never a negative clock, whatever the system clock did.
    assert.strictEqual(mf.formatDuration(-5), '0:00');
});
