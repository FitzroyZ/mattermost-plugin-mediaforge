//go:build !unix

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

// isolateProcessGroup is a no-op outside Unix.
//
// The plugin is built for Windows because the manifest declares that target,
// but process-group isolation has no portable equivalent there. Killing the
// process itself is what this platform offers, and it is what
// signalProcessGroup falls back to. A Windows deployment therefore has a
// weaker guarantee about helper processes than a Linux one — which is worth
// stating plainly rather than hiding behind a stub that looks equivalent.
func isolateProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup kills the child process itself.
func signalProcessGroup(cmd *exec.Cmd, _ syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

// terminateSignal and killSignal exist only to keep the call sites identical
// across platforms; neither value is inspected here.
const (
	terminateSignal = syscall.Signal(0)
	killSignal      = syscall.Signal(0)
)

// freeBytesOn has no portable implementation outside Unix.
//
// The health report treats the error as "free space could not be determined"
// and still reports the directory as writable, which is the honest answer
// here: a wrong number would be worse than none.
func freeBytesOn(_ string) (uint64, error) {
	return 0, errors.ErrUnsupported
}
