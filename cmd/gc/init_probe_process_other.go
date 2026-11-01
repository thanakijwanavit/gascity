//go:build !linux

package main

import "os/exec"

// setInitProbeParentDeathSignal is a no-op off Linux: Pdeathsig is
// Linux-only, so probes there rely on exec.CommandContext alone.
func setInitProbeParentDeathSignal(*exec.Cmd) {}
