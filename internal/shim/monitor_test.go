package shim

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestMemMonitorKillsOverLimit verifies the RSS monitor kills a process group
// that exceeds its mem_gb limit and records the violation (the mrp --monitor
// analog). A child that allocates ~200 MB is capped at 30 MB.
func TestMemMonitorKillsOverLimit(t *testing.T) {
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
	if groupRSS(cmd.Process.Pid) != 0 {
		t.Log("note: group RSS non-zero after exit is fine (best-effort read)")
	}
}
