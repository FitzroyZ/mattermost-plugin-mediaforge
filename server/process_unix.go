//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts the child into its own process group.
//
// This is what makes a timeout enforceable. ffmpeg runs worker threads and may
// itself spawn helpers; signalling only the parent PID can leave the group
// alive with the output file still open, which is the orphaned-process failure
// this slice is required to rule out. A child in its own group can be
// signalled as a whole with a single negative PID.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup sends a signal to every process in the child's group.
//
// The negative PID is the group. This is the part that must not be simplified
// to cmd.Process.Kill(): that signals one process and leaves the rest running.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// terminateSignal asks a process to shut down cleanly, so ffmpeg gets a chance
// to close its output file before it is killed outright.
const terminateSignal = syscall.SIGTERM

// killSignal cannot be caught or ignored.
const killSignal = syscall.SIGKILL

// freeBytesOn reports the space available to an unprivileged writer on the
// filesystem holding dir.
//
// Bavail rather than Bfree: Bfree counts the blocks reserved for root, which
// the Mattermost server cannot use. Reporting those would tell an
// administrator there is room when there is not.
func freeBytesOn(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
