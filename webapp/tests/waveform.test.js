// Copyright (c) 2026 MediaForge contributors.
// See LICENSE for license information.

// Contract tests for the waveform work: the waveform, the duration limit, and the
// per-channel button.
//
// They run under plain `node --test` like the voice recording tests next to them, with
// no dependencies at all. A waveform is normally where a chart library gets
// added to a project; it is not needed. The Web Audio API measures the signal
// and a canvas draws it, so what is testable here is the part that matters and
// the part a screenshot cannot prove: that the levels come from the REAL
// microphone stream and change while someone is speaking.

const assert = require('node:assert');
const test = require('node:test');

const mf = require('../src/mediaforge.js');

// --------------------------------------------------------------- test doubles

// fakeAnalyser stands in for an AnalyserNode. It writes whatever samples the
// test scripted into the caller's buffer, which is how a test can make the
// "microphone" loud, quiet, or silent on demand.
class fakeAnalyser {
    constructor() {
        this.fftSize = 2048;
        this.smoothingTimeConstant = 0;
        this.connected = [];
        this.disconnected = false;
        // Byte time-domain data is centred on 128; 128 means silence.
        this.level = 0;
        this.reads = 0;
    }
    getByteTimeDomainData(buffer) {
        this.reads++;
        for (let i = 0; i < buffer.length; i++) {
            const swing = Math.round(this.level * 127 * Math.sin((i / buffer.length) * Math.PI * 8));
            buffer[i] = 128 + swing;
        }
    }
    disconnect() {
        this.disconnected = true;
    }
}

class fakeSourceNode {
    constructor(stream) {
        this.stream = stream;
        this.connectedTo = null;
        this.disconnected = false;
    }
    connect(node) {
        this.connectedTo = node;
    }
    disconnect() {
        this.disconnected = true;
    }
}

class fakeAudioContext {
    constructor() {
        this.analysers = [];
        this.sources = [];
        this.closed = false;
        this.state = 'running';
        fakeAudioContext.instances.push(this);
    }
    createAnalyser() {
        const a = new fakeAnalyser();
        this.analysers.push(a);
        return a;
    }
    createMediaStreamSource(stream) {
        const s = new fakeSourceNode(stream);
        this.sources.push(s);
        return s;
    }
    close() {
        this.closed = true;
        this.state = 'closed';
        return Promise.resolve();
    }
}
fakeAudioContext.instances = [];

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

class fakeRecorder {
    constructor(stream, options) {
        this.stream = stream;
        this.mimeType = (options && options.mimeType) || 'audio/webm;codecs=opus';
        this.started = false;
        this.ondataavailable = null;
        this.onstop = null;
        fakeRecorder.instances.push(this);
    }
    static isTypeSupported(type) {
        return fakeRecorder.supported.includes(type);
    }
    start() {
        this.started = true;
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

function newModel(overrides) {
    fakeRecorder.instances = [];
    fakeAudioContext.instances = [];
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
        createObjectURL: () => 'blob:x',
        revokeObjectURL: () => {},
        AudioContext: fakeAudioContext,
        namePrefix: 'mediaforge-voice-',
        now: () => clock,
    }, overrides || {});

    const model = mf.createRecorderModel(deps);
    return {
        model,
        streams,
        advance: (ms) => {
            clock += ms;
        },
    };
}

// ------------------------------------------------------------- the waveform

test('the levels come from the real microphone stream, not from a generator',
    async () => {
        // This is the one thing about a waveform that a screenshot cannot
        // prove and that matters most: a bar chart driven by Math.random()
        // looks exactly like a working one. The analyser must be fed by the
        // stream getUserMedia returned.
        const h = newModel();
        await h.model.start();

        assert.strictEqual(fakeAudioContext.instances.length, 1,
            'no audio analysis was set up for the recording');
        const ctx = fakeAudioContext.instances[0];

        assert.strictEqual(ctx.sources.length, 1, 'the microphone was never analysed');
        assert.strictEqual(ctx.sources[0].stream, h.streams[0],
            'the analyser is not connected to the stream that was recorded');
        assert.strictEqual(ctx.sources[0].connectedTo, ctx.analysers[0],
            'the microphone is not connected to the analyser');
    });

test('the level changes while someone is speaking', async () => {
    // The waveform's whole purpose is telling a person that the microphone
    // really hears them. So the number behind it has to move with the signal.
    const h = newModel();
    await h.model.start();
    const analyser = fakeAudioContext.instances[0].analysers[0];

    analyser.level = 0;
    const silent = h.model.sampleLevel();

    analyser.level = 0.9;
    const loud = h.model.sampleLevel();

    analyser.level = 0.2;
    const quiet = h.model.sampleLevel();

    assert.ok(silent < 0.05, `silence measured ${silent}`);
    assert.ok(loud > 0.3, `a loud signal measured ${loud}`);
    assert.ok(loud > quiet && quiet > silent,
        `the level does not follow the signal: silent=${silent} quiet=${quiet} loud=${loud}`);
});

test('the level is always inside the range a drawing can use', async () => {
    const h = newModel();
    await h.model.start();
    const analyser = fakeAudioContext.instances[0].analysers[0];

    for (const level of [0, 0.5, 1, 2]) {
        analyser.level = level;
        const got = h.model.sampleLevel();
        assert.ok(got >= 0 && got <= 1,
            `a level of ${level} produced ${got}, which is outside 0..1`);
    }
});

test('the recent levels are kept so a waveform can be drawn from them',
    async () => {
        // A waveform is a history, not a single bar. The model keeps it so the
        // component can draw without owning any state of its own.
        const h = newModel();
        await h.model.start();
        const analyser = fakeAudioContext.instances[0].analysers[0];

        for (const level of [0.1, 0.9, 0.3, 0.7]) {
            analyser.level = level;
            h.model.sampleLevel();
        }

        const levels = h.model.recentLevels();
        assert.ok(Array.isArray(levels), 'the levels are not readable as a list');
        assert.ok(levels.length >= 4, `only ${levels.length} level(s) were kept`);

        const lastFour = levels.slice(-4);
        assert.ok(lastFour[1] > lastFour[0], 'the history does not follow the signal');
    });

test('the history is bounded, so a long recording cannot grow without limit',
    async () => {
        // Ten minutes at 20 samples a second is 12 000 numbers. The drawing
        // only ever shows the last stretch, so keeping them all would be a
        // leak in exchange for nothing.
        const h = newModel();
        await h.model.start();
        const analyser = fakeAudioContext.instances[0].analysers[0];

        for (let i = 0; i < 5000; i++) {
            analyser.level = (i % 10) / 10;
            h.model.sampleLevel();
        }

        assert.ok(h.model.recentLevels().length <= 256,
            `${h.model.recentLevels().length} levels were kept from 5000 samples`);
    });

test('the audio analysis is torn down on every path that ends a recording',
    async () => {
        // An AudioContext holds the sound card open. Leaving one behind per
        // recording is the same class of failure as leaving the microphone
        // track live: the browser keeps showing that the page is using audio.
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

            const open = fakeAudioContext.instances.filter((c) => !c.closed);
            assert.strictEqual(open.length, 0,
                `${ending} left ${open.length} audio context(s) open`);
        }
    });

test('a browser without the Web Audio API still records', async () => {
    // The waveform is a comfort, the recording is the feature. A browser that
    // cannot analyse must still be able to speak into the microphone.
    const h = newModel({AudioContext: null});
    await h.model.start();

    assert.strictEqual(h.model.state.phase, 'recording');
    assert.strictEqual(h.model.sampleLevel(), 0,
        'a browser with no analyser reported a level it cannot have measured');

    const file = await h.model.stop();
    assert.ok(file, 'no recording was produced without the Web Audio API');
});

// --------------------------------------------------------- the duration limit

test('the recording stops itself at the limit rather than being refused later',
    async () => {
        // The server enforces the limit because an upload can arrive without
        // this webapp. But letting the browser run past it and then refusing
        // the upload would waste the user's words: they spoke for eleven
        // minutes and get "too long" at the end of it.
        const h = newModel({maxSeconds: 5});
        await h.model.start();

        h.advance(4000);
        assert.strictEqual(h.model.overTheLimit(), false,
            'a recording under the limit was reported as over it');

        h.advance(2000);
        assert.strictEqual(h.model.overTheLimit(), true,
            'a recording past the limit was not recognised');
    });

test('the limit comes from the server, not from a number in the JavaScript',
    async () => {
        // Same reasoning as the name prefix in voice recording: two copies of one rule
        // drift apart, and the symptom here would be a browser that stops at
        // ten minutes while the server refuses at five.
        const h = newModel({maxSeconds: 120});
        await h.model.start();

        h.advance(119000);
        assert.strictEqual(h.model.overTheLimit(), false);
        h.advance(2000);
        assert.strictEqual(h.model.overTheLimit(), true);
    });

test('with no limit configured the recording is not cut short', async () => {
    const h = newModel({maxSeconds: 0});
    await h.model.start();
    h.advance(3600000);
    assert.strictEqual(h.model.overTheLimit(), false,
        'a recording was stopped although no limit was configured');
});

test('the remaining time is what a person needs to see', async () => {
    const h = newModel({maxSeconds: 60});
    await h.model.start();
    h.advance(45000);

    assert.strictEqual(Math.round(h.model.remainingSeconds()), 15);

    h.advance(30000);
    assert.strictEqual(h.model.remainingSeconds(), 0,
        'the remaining time went negative');
});

// ------------------------------------------------------ the per-channel button

test('the button asks about the channel it is actually in', async () => {
    // Measured before this slice started: the upload callback from
    // registerFileUploadMethod is called with NOTHING but the callback
    // (file_upload.tsx line 659), while the post editor component receives the
    // whole PostDraft, which carries channelId. So a per-channel button has to
    // be driven from the draft.
    let asked = null;
    await mf.fetchVoiceSettings((url) => {
        asked = url;
        return Promise.resolve({ok: true, json: () => Promise.resolve({enabled: true})});
    }, 'kchannelid00000000000000000');

    assert.ok(asked.includes('channel_id=kchannelid00000000000000000'),
        `the settings were asked for without a channel: ${asked}`);
});

test('a channel the server did not open up draws no button', async () => {
    // The rule this slice must not break: a button that offers something the
    // server then refuses is worse than no button at all.
    const Recorder = mf.createRecorderComponent(
        stubReact(), newModel().model, stubUpload(), {enabled: false});

    const rendered = render(Recorder, {draft: {channelId: 'blockedchannel'}});
    assert.strictEqual(rendered, null,
        'a record button was drawn in a channel where recording is not available');
});

test('a channel the server opened up draws the button', async () => {
    const Recorder = mf.createRecorderComponent(
        stubReact(), newModel().model, stubUpload(), {enabled: true});

    const rendered = render(Recorder, {draft: {channelId: 'openchannel'}});
    assert.notStrictEqual(rendered, null, 'no record button was drawn');
});

// ----------------------------------------------------------- the drawing

test('the bars are never painted in a colour the canvas cannot use', () => {
    // A REGRESSION TEST FOR A DEFECT THIS SLICE SHIPPED AND THEN FOUND.
    //
    // The first version set `ctx.fillStyle = 'currentColor'`, reasoning that
    // the bars should inherit the message box's text colour. A canvas is not
    // CSS: `currentColor` is not a value it understands, and assigning it
    // leaves fillStyle at BLACK — measured in Chromium, which reports
    // '#000000' afterwards. On Mattermost's dark theme that paints a black
    // waveform on a near-black background, which is invisible.
    //
    // Nothing in the code looked wrong and every other test passed. It was
    // caught by rendering the component and LOOKING at the picture.
    const painted = [];
    const canvas = {
        width: 96,
        height: 24,
        getContext: () => ({
            clearRect: () => {},
            fillRect: () => {},
            set fillStyle(value) {
                painted.push(value);
            },
            get fillStyle() {
                return painted[painted.length - 1];
            },
        }),
    };

    mf.drawWaveform(canvas, [0.2, 0.8, 0.4]);

    assert.ok(painted.length > 0, 'no colour was ever set');
    for (const colour of painted) {
        assert.notStrictEqual(colour, 'currentColor',
            'the bars are painted in a value a canvas silently turns into black');
        assert.ok(/^(#[0-9a-f]{3,8}|rgba?\()/i.test(colour),
            `the bars are painted in ${colour}, which is not a colour a canvas accepts`);
    }
});

test('drawing survives a canvas that is not there yet', () => {
    // React hands a ref before the element exists on the first render, and a
    // throw inside the recording timer would stop the clock as well as the
    // picture.
    assert.doesNotThrow(() => mf.drawWaveform(null, [0.5]));
    assert.doesNotThrow(() => mf.drawWaveform({}, [0.5]));
    assert.doesNotThrow(() => mf.drawWaveform({getContext: () => null}, [0.5]));
});

// ------------------------------------------------- registration and channels

test('a channel list does not remove the button from the whole server',
    async () => {
        // The trap this pins: the plugin registers once, at load time, when
        // it does not yet know which channel anything is in. Asked without a
        // channel while a list is configured, the server correctly answers
        // "not available here" — and reading THAT as "register nothing" would
        // remove the button from the opened-up channels as well, which is the
        // opposite of what the setting is for.
        //
        // The name prefix is what says "this server does voice messages", so
        // that is what the registration reads.
        const registered = {methods: 0, components: 0};
        const registry = {
            registerFileUploadMethod: () => {
                registered.methods++;
                return 'id';
            },
            registerPostEditorActionComponent: () => {
                registered.components++;
            },
        };

        await new mf.MediaForgePlugin().initialize(registry, {}, {
            fetch: () => Promise.resolve({
                ok: true,
                json: () => Promise.resolve({
                    // What the server answers a channel-less question when a
                    // channel list is configured.
                    enabled: false,
                    name_prefix: 'mediaforge-voice-',
                    max_seconds: 600,
                }),
            }),
            React: stubReact(),
            window: {},
            getUserMedia: () => Promise.resolve(new fakeStream()),
            MediaRecorder: fakeRecorder,
            Blob: fakeBlob,
            File: fakeFile,
            createObjectURL: () => 'blob:x',
            revokeObjectURL: () => {},
        });

        // The component is what carries the mic button now that the
        // attachment-menu entry is gone (PLAN, the voice send fix), so it is the
        // registration this property is read from.
        assert.strictEqual(registered.components, 1,
            'a configured channel list removed the button from every channel');
        assert.strictEqual(registered.methods, 0,
            'an attachment-menu entry was registered; it was removed on purpose');
        assert.strictEqual(registered.components, 1);
    });

test('a server with voice messages switched off still registers nothing',
    async () => {
        // The other direction, and the one that must not be broken by the
        // rule above: a server that does not offer the feature at all sends
        // no prefix, and the message box stays exactly as it is without the
        // plugin.
        const registry = {
            registerFileUploadMethod: () => assert.fail('a button was registered anyway'),
            registerPostEditorActionComponent: () => assert.fail('a component was registered anyway'),
        };
        await new mf.MediaForgePlugin().initialize(registry, {}, {
            fetch: () => Promise.resolve({
                ok: true,
                json: () => Promise.resolve({enabled: false, name_prefix: '', max_seconds: 0}),
            }),
            React: stubReact(),
            window: {},
        });
    });

test('moving to another channel re-asks rather than trusting the first answer',
    async () => {
        // A person switches channel without reloading the page. If the answer
        // were cached per session the button would follow them into a channel
        // it does not belong in.
        const asked = [];
        const availability = mf.createAvailability((channelId) => {
            asked.push(channelId);
            return Promise.resolve({enabled: channelId === 'open', max_seconds: 600});
        });

        await availability.forChannel('open');
        await availability.forChannel('blocked');

        assert.deepStrictEqual(asked, ['open', 'blocked'],
            'the second channel was answered from the first channel\'s answer');
    });

// ------------------------------------------------------------------ helpers

function stubReact() {
    return {
        createElement: (type, props, ...children) => ({type, props, children}),
        useState: (initial) => [initial, () => {}],
        useEffect: () => {},
        useRef: (initial) => ({current: initial}),
    };
}

function stubUpload() {
    return {available: () => true, send: () => {}};
}

function render(Component, props) {
    return Component(props);
}
