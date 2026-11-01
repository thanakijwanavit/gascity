//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestInitProbeChildDiesWithHardParentExit pins g1-tnbbv: a probe prepared by
// setInitProbeParentDeathSignal must not outlive a gc that exits without any
// cleanup (the ssh-dropped `gc init` case that left `dolt config --global`
// orphans spinning under PID 1). The harness re-execs this test binary,
// starts `sleep` as the probe, reports its pid and hard-exits.
func TestInitProbeChildDiesWithHardParentExit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "probe.pid")
	cmd := exec.Command(exe, "-test.run=^TestInitProbeHardExitHarness$", "--")
	cmd.Env = append(os.Environ(),
		"INIT_PROBE_HARNESS=1",
		"INIT_PROBE_PIDFILE="+pidFile,
	)
	out, runErr := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		t.Fatalf("run harness: %v\n%s", runErr, out)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("harness did not report a probe pid (output below):\n%s\nerr: %v", out, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse pidfile %q: %v", pidBytes, err)
	}
	// Pdeathsig delivery is asynchronous; poll rather than assert at once.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // never leak the reproduction
	t.Fatalf("probe pid %d still alive 5s after gc hard-exited; Pdeathsig is not set on init probes", pid)
}

// TestInitProbeHardExitHarness is the re-exec target for the test above. Its
// env keys avoid the GC_ prefix, which TestMain scrubs.
func TestInitProbeHardExitHarness(t *testing.T) {
	if os.Getenv("INIT_PROBE_HARNESS") != "1" {
		t.Skip("harness process")
	}
	pidFile := os.Getenv("INIT_PROBE_PIDFILE")
	if pidFile == "" {
		t.Fatal("INIT_PROBE_PIDFILE not set")
	}
	probe := exec.Command("sleep", "300")
	setInitProbeParentDeathSignal(probe)
	if err := probe.Start(); err != nil {
		t.Fatalf("start probe: %v", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(probe.Process.Pid)), 0o600); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	// Hard exit: no Wait, no kill, no deferred cleanup.
	os.Exit(0)
}

// TestSetInitProbeParentDeathSignalKeepsExistingAttrs checks the helper adds
// Pdeathsig without dropping attributes a caller (or a test override of
// initRunVersionCommandContext) already set.
func TestSetInitProbeParentDeathSignalKeepsExistingAttrs(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setInitProbeParentDeathSignal(cmd)
	if !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("SysProcAttr = %+v, want Setpgid kept and Pdeathsig SIGKILL", cmd.SysProcAttr)
	}
	bare := exec.Command("true")
	setInitProbeParentDeathSignal(bare)
	if bare.SysProcAttr == nil || bare.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("bare SysProcAttr = %+v, want Pdeathsig SIGKILL", bare.SysProcAttr)
	}
}
