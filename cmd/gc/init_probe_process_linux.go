//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setInitProbeParentDeathSignal asks the kernel to SIGKILL a gc init probe
// (dolt config --global --get, <binary> version) if gc itself dies first.
//
// exec.CommandContext only kills the probe while gc is alive to cancel the
// context. When gc is killed instead (an ssh session that ran `gc init`
// drops, a SIGKILL from a supervisor), a hung probe is reparented to PID 1
// and spins forever: on G1 31 Oct 2026 21 orphaned
// `dolt config --global --get user.name` processes ran at ~99% CPU each and
// pushed load from 99 to 234 (g1-tnbbv). Pdeathsig is kernel-enforced and
// fires no matter how gc exits. Mirrors prepareCommandForTimeout in
// internal/beads.
func setInitProbeParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
