// Copyright (c) 2026 MediaForge contributors.
// See LICENSE for license information.

// the file-naming work.1: the sender is told why they are waiting.
//
// A video transcode holds the upload open for minutes (2.5 min for 306 MB on two
// cores, SPEC §6.1) and the sender was told nothing at all — the send button was
// simply unavailable, which reads as a broken client rather than as work in
// progress.
//
// The signal is `draft.uploadsInProgress`, the webapp's own list of uploads it is
// waiting for, handed to this component as a prop.

const assert = require('node:assert');
const test = require('node:test');

const mf = require('../src/mediaforge.js');

function stubReact() {
    const states = [];
    let cursor = 0;
    return {
        createElement: (type, props, ...children) => ({type, props, children}),
        useState(initial) {
            const i = cursor++;
            if (!(i in states)) {
                states[i] = initial;
            }
            return [states[i], (next) => {
                states[i] = next;
            }];
        },
        useEffect: () => {},
        useRef: (initial) => ({current: initial}),
        _reset() {
            cursor = 0;
        },
    };
}

function idleModel() {
    return {
        state: {phase: 'idle', error: '', previewUrl: '', file: null},
        supported: () => true,
        maxSeconds: () => 600,
        remainingSeconds: () => 600,
        elapsedSeconds: () => 0,
        recentLevels: () => [],
        overTheLimit: () => false,
        start: () => Promise.resolve(),
        stop: () => Promise.resolve(),
        discard: () => Promise.resolve(),
        take: () => null,
        restore: () => {},
        sampleLevel: () => {},
    };
}

function render(draft, opts = {}) {
    const react = stubReact();
    const model = opts.model || idleModel();
    const Recorder = mf.createRecorderComponent(
        react, model,
        {available: () => true, send: () => true},
        {enabled: opts.enabled === undefined ? true : opts.enabled});
    react._reset();
    return Recorder({draft});
}

function textOf(tree) {
    return JSON.stringify(tree || null);
}

test('a waiting sender is told that compression is running', () => {
    const tree = render({channelId: 'c', uploadsInProgress: ['upload-1']});
    assert.match(textOf(tree), /Compressing/,
        'the sender waiting for a transcode is told nothing, which is the ' +
        'complaint this slice answers');
});

test('nothing is said when no upload is pending', () => {
    const tree = render({channelId: 'c', uploadsInProgress: []});
    assert.ok(!/Compressing/.test(textOf(tree)),
        'a "compressing" hint was shown with no upload in flight');
});

test('a missing uploadsInProgress is treated as nothing pending', () => {
    // Older servers, or a draft shape that simply omits the field: the hint must
    // not appear, and reading the field must not throw.
    assert.doesNotThrow(() => render({channelId: 'c'}));
    assert.ok(!/Compressing/.test(textOf(render({channelId: 'c'}))));
});

test('several pending uploads are counted', () => {
    const tree = render({channelId: 'c', uploadsInProgress: ['a', 'b', 'c']});
    assert.match(textOf(tree), /3 files/,
        'the count is not shown, so a sender cannot tell one slow file from three');
});

test('the hint appears even where voice messages are switched off', () => {
    // Compression applies to every image and video upload whether or not voice
    // is enabled. Gating the hint behind the voice check would hide it from
    // exactly the installations that only use compression.
    const tree = render(
        {channelId: 'c', uploadsInProgress: ['upload-1']}, {enabled: false});
    assert.match(textOf(tree), /Compressing/,
        'the waiting hint is hidden on a server that has voice messages off, ' +
        'which is most of them');
});

test('the hint appears even in a browser that cannot record', () => {
    const model = idleModel();
    model.supported = () => false;
    const tree = render({channelId: 'c', uploadsInProgress: ['upload-1']}, {model});
    assert.match(textOf(tree), /Compressing/,
        'an old browser gets no waiting hint, though it uploads files like any other');
});

test('a recording in progress is not replaced by the hint', () => {
    // An upload finishing while someone is speaking is ordinary. Swapping the
    // live recorder for "Compressing…" would take away the stop button
    // mid-sentence and lose what they had said.
    const model = idleModel();
    model.state.phase = 'recording';
    model.remainingSeconds = () => 590;
    const tree = render({channelId: 'c', uploadsInProgress: ['upload-1']}, {model});
    const text = textOf(tree);

    assert.ok(!/Compressing/.test(text),
        'the waiting hint replaced an active recording, removing the stop button');
    assert.match(text, /Stop recording/,
        'the stop button disappeared while recording');
});

test('the hint sits in a row that cannot wrap out of the toolbar', () => {
    // Same property as every other phase, for the same reason: the toolbar clips
    // what does not fit.
    const tree = render({channelId: 'c', uploadsInProgress: ['upload-1']});
    const style = (tree && tree.props && tree.props.style) || {};
    assert.strictEqual(style.whiteSpace, 'nowrap',
        'the waiting row may wrap and be clipped');
    assert.strictEqual(style.flexShrink, 0,
        'the waiting row can be squeezed into a sliver');
});

test('the hint is announced to a screen reader', () => {
    const tree = render({channelId: 'c', uploadsInProgress: ['upload-1']});
    assert.strictEqual(tree.props['aria-live'], 'polite',
        'a sender using a screen reader is not told that anything is happening');
});
