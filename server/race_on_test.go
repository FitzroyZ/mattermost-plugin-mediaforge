//go:build race

package main

// raceSlowdown is how much longer a test may take when the race detector is
// on.
//
// The detector instruments every memory access, and the image pipeline is
// nothing but memory access: decoding, resampling and re-encoding a photo runs
// several times slower under it. Without this, a correct implementation fails
// its own deadline on the race run — a failure about the harness rather than
// about the code, and the worst kind, because it looks like a hang.
//
// Measured on the reference container rather than guessed: the note suite
// needs roughly five times as long under -race.
const raceSlowdown = 6
