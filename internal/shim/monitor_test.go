package shim

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireLinux skips a test that depends on /proc and Unix process groups.
func requireLinux(t *testing.T) {
	t.Helper()

	if runtime.GOOS != "linux" {
		t.Skip("memory monitor is Linux-only (/proc + process groups)")
	}
}

// TestMemMonitorKillsOverLimit verifies the RSS monitor kills a process group
// that exceeds its mem_gb limit and records the violation (the mrp --monitor
// analog). A child that allocates ~200 MB is capped at 30 MB.
func TestMemMonitorKillsOverLimit(t *testing.T) {
	requireLinux(t)

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	cmd := exec.Command("python3", "-c", "x = bytearray(200*1024*1024)\nimport time; time.sleep(30)")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	mon := &memMonitor{limitBytes: 30 * 1024 * 1024, pgid: cmd.Process.Pid}
	go mon.watch(t.Context())

	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatal("monitor did not kill the over-limit process within 10s")
	}

	if !mon.violated.Load() {
		t.Error("monitor did not record a violation")
	}
	if mon.peakBytes.Load() < mon.limitBytes {
		t.Errorf("recorded peak %d not above the limit %d", mon.peakBytes.Load(), mon.limitBytes)
	}
	if mon.message() == "" {
		t.Error("empty violation message")
	}
}

// TestMemMonitorAllowsUnderLimit verifies a process that stays under its limit
// runs to completion without a spurious kill.
func TestMemMonitorAllowsUnderLimit(t *testing.T) {
	requireLinux(t)

	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	cmd := exec.Command("sleep", "2")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	mon := &memMonitor{limitBytes: 8 * bytesPerGB, pgid: cmd.Process.Pid}
	go mon.watch(t.Context())

	if err := cmd.Wait(); err != nil {
		t.Errorf("under-limit process should exit normally, got %v", err)
	}
	if mon.violated.Load() {
		t.Error("monitor falsely flagged a tiny process")
	}
}

// TestMemMonitorRecordExitPeak verifies the post-hoc rusage check catches a peak
// that lived and died between samples: a process allocates ~150 MB then frees it
// and exits immediately (too fast for the 1s poll), but wait4's ru_maxrss records
// the peak, so a low limit still trips.
func TestMemMonitorRecordExitPeak(t *testing.T) {
	requireLinux(t)

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}

	cmd := exec.Command("python3", "-c", "x = bytearray(150*1024*1024); del x")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	low := &memMonitor{limitBytes: 20 * 1024 * 1024}
	low.recordExitPeak(cmd.ProcessState)
	if !low.violated.Load() {
		t.Error("recordExitPeak missed the exit-time RSS peak")
	}

	high := &memMonitor{limitBytes: 8 * bytesPerGB}
	high.recordExitPeak(cmd.ProcessState)
	if high.violated.Load() {
		t.Errorf("recordExitPeak falsely flagged a %d-byte peak under an 8 GB limit", high.peakBytes.Load())
	}

	// nil receiver / nil state must be safe no-ops.
	(*memMonitor)(nil).recordExitPeak(cmd.ProcessState)
	high.recordExitPeak(nil)
}

// TestProcPgrp covers the /proc parse and the kill guard's discriminator: our own
// pid reports our process group, and an absent pid reports -1 (which the guard
// uses to skip killing a reaped/recycled leader).
func TestProcPgrp(t *testing.T) {
	requireLinux(t)

	if got, want := procPgrp(os.Getpid()), syscall.Getpgrp(); got != want {
		t.Errorf("procPgrp(self) = %d, want %d", got, want)
	}
	if got := procPgrp(1 << 30); got != -1 {
		t.Errorf("procPgrp(absent pid) = %d, want -1", got)
	}
}

// TestMemMonitorMessage locks the failure text to Martian's canonical prefix so
// tooling that greps _errors for it matches mre's message too.
func TestMemMonitorMessage(t *testing.T) {
	m := &memMonitor{limitBytes: 8 * bytesPerGB}
	m.peakBytes.Store(9 * bytesPerGB)

	if msg := m.message(); !strings.HasPrefix(msg, "Stage exceeded its memory quota") {
		t.Errorf("message %q must lead with Martian's canonical prefix", msg)
	}
}
