// Copyright (c) 2026 MediaForge contributors.
// See LICENSE for license information.

// Contract tests for the System Console health section.
//
// The section exists because a missing ffmpeg used to be invisible: the server
// wrote a line to its log and the console showed nothing, so an installation
// with no video compression looked exactly like a healthy one.
//
// These tests pin the two things that decide whether it is worth having: that
// an error is actually shown as an error rather than folded into a friendly
// summary, and that the section fails VISIBLY when it cannot reach the server.
// A health display that goes quiet when the request fails is worse than none,
// because silence is what it is supposed to replace.

const assert = require('node:assert');
const test = require('node:test');

const mf = require('../src/mediaforge.js');

// respondWith builds a fetch stand-in returning one canned answer.
function respondWith(payload, ok) {
    return function () {
        return Promise.resolve({
            ok: ok !== false,
            json: function () {
                return Promise.resolve(payload);
            },
        });
    };
}

test('the health report is read from the plugin route', async function () {
    let seenUrl = null;
    const fetchImpl = function (url) {
        seenUrl = url;
        return Promise.resolve({
            ok: true,
            json: function () {
                return Promise.resolve({healthy: true, checks: []});
            },
        });
    };

    await mf.fetchHealth(fetchImpl);

    assert.match(seenUrl, /\/plugins\/com\.github\.berzionline\.mediaforge\/health$/);
});

test('a healthy report is reported as healthy', async function () {
    const report = await mf.fetchHealth(respondWith({
        healthy: true,
        checks: [{id: 'ffmpeg', label: 'ffmpeg', status: 'ok', detail: '/usr/bin/ffmpeg — 7.1.5'}],
    }));

    assert.equal(report.healthy, true);
    assert.equal(report.checks.length, 1);
    assert.equal(report.checks[0].status, 'ok');
});

test('an unreachable server is shown as an error, not as silence', async function () {
    // This is the failure mode that matters. If the request fails and the
    // section renders nothing, the administrator is back to the situation the
    // section was built to end: no information, and no sign that information
    // is missing.
    const report = await mf.fetchHealth(function () {
        return Promise.reject(new Error('network down'));
    });

    assert.equal(report.healthy, false);
    assert.ok(report.checks.length > 0, 'a failed request must still produce a visible row');
    assert.equal(report.checks[0].status, 'error');
    assert.match(report.checks[0].detail, /could not be read|could not be reached/i);
});

test('an HTTP error is shown as an error', async function () {
    const report = await mf.fetchHealth(respondWith({}, false));

    assert.equal(report.healthy, false);
    assert.equal(report.checks[0].status, 'error');
});

test('a report with an error check is not healthy', async function () {
    const report = await mf.fetchHealth(respondWith({
        healthy: false,
        checks: [
            {id: 'ffmpeg', label: 'ffmpeg', status: 'error', detail: 'not found. Install it...'},
            {id: 'spool', label: 'Temporary working directory', status: 'ok', detail: '/tmp — 40 GB free.'},
        ],
    }));

    assert.equal(report.healthy, false);
    const ffmpeg = report.checks.find(function (c) {
        return c.id === 'ffmpeg';
    });
    assert.equal(ffmpeg.status, 'error');
    // The remedy has to survive the trip to the browser: the whole point is
    // that the administrator can act without reading the server log.
    assert.match(ffmpeg.detail, /Install/);
});

test('the status summary counts the errors it found', function () {
    const summary = mf.summariseHealth({
        healthy: false,
        checks: [
            {id: 'ffmpeg', status: 'error'},
            {id: 'ffprobe', status: 'error'},
            {id: 'encoder', status: 'warn'},
            {id: 'spool', status: 'ok'},
        ],
    });

    assert.equal(summary.errors, 2);
    assert.equal(summary.warnings, 1);
    assert.equal(summary.ok, false);
});

test('an all-clear summary says so', function () {
    const summary = mf.summariseHealth({
        healthy: true,
        checks: [{id: 'ffmpeg', status: 'ok'}, {id: 'spool', status: 'ok'}],
    });

    assert.equal(summary.errors, 0);
    assert.equal(summary.warnings, 0);
    assert.equal(summary.ok, true);
});

test('a malformed answer does not throw', function () {
    // The server is the only thing that produces this payload, but a version
    // mismatch between a cached webapp bundle and a newer server is exactly
    // the situation where a health display must not be the thing that breaks.
    assert.doesNotThrow(function () {
        mf.summariseHealth(null);
        mf.summariseHealth({});
        mf.summariseHealth({checks: 'not a list'});
    });
});
