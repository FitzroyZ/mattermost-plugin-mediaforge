// Copyright (c) 2026 MediaForge contributors.
// See LICENSE for license information.

// The gate that was missing when the voice feature shipped.
//
// earlier work asserted that `upload.send` is CALLED with the right file.
// Nothing asserted that a human can REACH the button that calls it — and that
// is exactly how the feature reached production unusable: the preview rendered
// a native `<audio controls>` element, ~300 px wide and not shrinkable, into
// the narrow message-box toolbar. It pushed the ✓ and 🗑 buttons out of sight.
//
// Every symptom followed from that: recording worked, playback worked, the
// recording could not be sent, Mattermost's own send button stayed grey, and
// the console stayed silent because nothing ever threw.
//
// These tests inspect the rendered tree rather than the behaviour of a stub,
// which is the one thing the existing suite structurally could not do.

const assert = require('node:assert');
const test = require('node:test');

const mf = require('../src/mediaforge.js');

// --------------------------------------------------------------- test doubles

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

// A model already holding a finished recording, i.e. the preview phase.
function previewModel() {
    return {
        state: {phase: 'preview', error: '', previewUrl: 'blob:fake', file: {}},
        supported: () => true,
        maxSeconds: () => 600,
        remainingSeconds: () => 590,
        elapsedSeconds: () => 0,
        recentLevels: () => [0.2, 0.5, 0.3],
        overTheLimit: () => false,
        start: () => Promise.resolve(),
        stop: () => Promise.resolve(),
        discard: () => Promise.resolve(),
        take: () => ({name: 'mediaforge-voice-20260919.webm'}),
        sampleLevel: () => {},
    };
}

function renderPreview() {
    const react = stubReact();
    const Recorder = mf.createRecorderComponent(
        react, previewModel(), {available: () => true, send: () => {}},
        {enabled: true});
    react._reset();
    return Recorder({draft: {channelId: 'testchannel'}});
}

// Walk the rendered tree and collect every node matching a predicate.
function collect(node, match, found = []) {
    if (!node || typeof node !== 'object') {
        return found;
    }
    if (Array.isArray(node)) {
        node.forEach((child) => collect(child, match, found));
        return found;
    }
    if (match(node)) {
        found.push(node);
    }
    (node.children || []).forEach((child) => collect(child, match, found));
    return found;
}

function labels(tree) {
    return collect(tree, (n) => n.type === 'button')
        .map((n) => (n.props && n.props['aria-label']) || '');
}

// ------------------------------------------------------------------- the gate

test('the preview offers a way to attach the recording', () => {
    const tree = renderPreview();
    assert.ok(labels(tree).includes('Attach this recording'),
        'the preview has no attach button, so a finished recording can never be sent');
});

test('the preview offers a way to discard the recording', () => {
    assert.ok(labels(renderPreview()).includes('Discard recording'),
        'the preview has no discard button');
});

test('the preview renders no native audio player', () => {
    // THE regression test. A native <audio controls> element is ~300 px wide
    // and does not shrink; in the message-box toolbar it pushed the attach and
    // discard buttons out of the visible area. The recording was then playable
    // but unsendable, with nothing in the console to explain why.
    const players = collect(renderPreview(), (n) => n.type === 'audio');
    assert.strictEqual(players.length, 0,
        'a native <audio controls> element is back in the toolbar; it overflows ' +
        'the narrow message box and clips the attach button out of view');
});

test('playback is offered as a toolbar-sized button instead', () => {
    // Removing the player must not remove the ability to hear the recording:
    // SPEC 4.3 requires preview, replay and discard before sending.
    const found = labels(renderPreview()).filter((l) => /play/i.test(l));
    assert.ok(found.length > 0,
        'nothing in the preview lets the recording be replayed before sending');
});

test('the attach button comes before anything that can grow wide', () => {
    // Order is the actual defence. Whatever else the preview shows, the two
    // controls that decide the recording's fate must not be last in a row that
    // can overflow.
    const tree = renderPreview();
    const buttons = labels(tree);
    const attach = buttons.indexOf('Attach this recording');
    const discard = buttons.indexOf('Discard recording');

    assert.ok(attach >= 0 && discard >= 0, 'the preview lost its buttons');
    assert.ok(attach < discard,
        'attach should precede discard so the primary action is reached first');
});

test('every preview control carries a label a screen reader can announce', () => {
    const unlabelled = collect(renderPreview(), (n) => n.type === 'button')
        .filter((n) => !(n.props && n.props['aria-label']));
    assert.strictEqual(unlabelled.length, 0,
        `${unlabelled.length} button(s) in the preview have no aria-label`);
});

// ------------------------------------------- every phase, not just one

// The lesson that cost a second round: the preview was fixed and the RECORDING
// phase was left untouched, so the operator saw the same clipped row again with
// different buttons in it. Testing one phase proves nothing about the others.

function renderPhase(phase) {
    const react = stubReact();
    const model = previewModel();
    model.state.phase = phase;
    if (phase === 'recording') {
        model.remainingSeconds = () => 590;
    }
    const Recorder = mf.createRecorderComponent(
        react, model, {available: () => true, send: () => true}, {enabled: true});
    react._reset();
    return Recorder({draft: {channelId: 'testchannel'}});
}

for (const phase of ['recording', 'preview']) {
    test(`the ${phase} row cannot wrap or be squeezed out of the toolbar`, () => {
        const tree = renderPhase(phase);
        const style = (tree && tree.props && tree.props.style) || {};

        assert.strictEqual(style.whiteSpace, 'nowrap',
            `the ${phase} row may wrap; in a narrow window its buttons drop ` +
            `out of the toolbar's clipped height and cannot be clicked`);
        assert.strictEqual(style.display, 'inline-flex',
            `the ${phase} row is not a flex row, so its children are not held ` +
            'on one line');
        assert.strictEqual(style.flexShrink, 0,
            `the ${phase} row can be squeezed by the toolbar into a sliver`);
    });

    test(`the ${phase} waveform has a fixed box`, () => {
        const canvases = collect(renderPhase(phase), (n) => n.type === 'canvas');
        assert.strictEqual(canvases.length, 1, `no waveform in the ${phase} row`);
        const style = canvases[0].props.style || {};
        assert.strictEqual(style.width, '96px',
            `the ${phase} canvas has no fixed width and can be stretched by ` +
            'the flex row, which draws a smeared waveform');
        assert.strictEqual(style.flexShrink, 0,
            `the ${phase} canvas can be squeezed to nothing`);
    });

    test(`the ${phase} row keeps its buttons reachable`, () => {
        const found = labels(renderPhase(phase));
        assert.ok(found.length >= 2,
            `the ${phase} row has ${found.length} button(s); every phase needs ` +
            'at least a primary action and a discard');
        assert.ok(found.includes('Discard recording'),
            `the ${phase} row offers no way to discard`);
    });
}

test('the countdown stays hidden until the limit is close', () => {
    // "0:03 / -9:57" from the first second made the row needlessly wide in a
    // toolbar with no width to spare, and told the user nothing: what matters is
    // the warning near the limit, not arithmetic from the start.
    const model = previewModel();
    model.state.phase = 'recording';

    model.remainingSeconds = () => 590;
    const far = stubReact();
    let Recorder = mf.createRecorderComponent(
        far, model, {available: () => true, send: () => true}, {enabled: true});
    far._reset();
    assert.ok(!JSON.stringify(Recorder({draft: {channelId: 'c'}})).includes('-9:50'),
        'the countdown is shown with ten minutes left, widening the row for nothing');

    model.remainingSeconds = () => 30;
    const near = stubReact();
    Recorder = mf.createRecorderComponent(
        near, model, {available: () => true, send: () => true}, {enabled: true});
    near._reset();
    assert.ok(JSON.stringify(Recorder({draft: {channelId: 'c'}})).includes('-0:30'),
        'the countdown is missing with 30 seconds left, when it is the one ' +
        'moment the user needs it');
});
