package shim

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// rssSampleInterval is how often the memory monitor samples RSS, matching
	// mrp's mrjob monitor cadence.
	rssSampleInterval = time.Second
	// statPgrpIndex is the process-group field's position in /proc/<pid>/stat,
	// counting from just after the comm field: state(0) ppid(1) pgrp(2).
	statPgrpIndex = 2
	// statmRSSIndex is the resident-pages field's position in /proc/<pid>/statm:
	// size(0) resident(1).
	statmRSSIndex = 1
)

// memMonitor watches a process group's resident memory and kills the group when
// it exceeds a limit — the analog of mrp's --monitor RSS kill (mrjob polls the
// stage process tree's RSS and kills it when it exceeds the mem_gb reservation,
// distinct from the RLIMIT_AS vmem cap). Unlike the vmem cap, this bounds real
// resident memory, which is what mrp's primary monitor enforces.
type memMonitor struct {
	limitBytes int64
	pgid       int
	violated   atomic.Bool
	peakBytes  atomic.Int64
}

// watch samples the process group's RSS every rssSampleInterval until ctx is
// cancelled. On breach it records the peak, SIGKILLs the whole group, and stops.
func (m *memMonitor) watch(ctx context.Context) {
	ticker := time.NewTicker(rssSampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rss := groupRSS(m.pgid)
			if rss > m.peakBytes.Load() {
				m.peakBytes.Store(rss)
			}

			if rss > m.limitBytes {
				m.violated.Store(true)
				_ = syscall.Kill(-m.pgid, syscall.SIGKILL)

				return
			}
		}
	}
}

// message renders the retryable failure text for a memory-quota kill, mirroring
// mrp's ExceededMemQuotaMessage so downstream logs read the same.
func (m *memMonitor) message() string {
	return fmt.Sprintf("stage exceeded its memory quota (using %.1f GB, allowed %.1f GB)",
		float64(m.peakBytes.Load())/bytesPerGB, float64(m.limitBytes)/bytesPerGB)
}

// groupRSS sums the resident set size (bytes) of every process in process group
// pgid, read from /proc. A process that exits mid-scan is skipped. Returns 0 when
// /proc is unavailable, so the monitor is a best-effort no-op off Linux (the
// prlimit vmem cap still applies where configured).
func groupRSS(pgid int) int64 {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}

	pageSize := int64(os.Getpagesize())

	var total int64

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		if procPgrp(pid) == pgid {
			total += procRSSPages(pid) * pageSize
		}
	}

	return total
}

// procPgrp returns the process-group id from /proc/<pid>/stat, or -1. The comm
// field can contain spaces and parentheses, so parsing starts after the last ')'.
func procPgrp(pid int) int {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return -1
	}

	s := string(data)

	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return -1
	}

	fields := strings.Fields(s[i+1:])
	if len(fields) <= statPgrpIndex {
		return -1
	}

	pgrp, err := strconv.Atoi(fields[statPgrpIndex])
	if err != nil {
		return -1
	}

	return pgrp
}

// procRSSPages returns the resident page count from /proc/<pid>/statm (field 2).
func procRSSPages(pid int) int64 {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "statm"))
	if err != nil {
		return 0
	}

	fields := strings.Fields(string(data))
	if len(fields) <= statmRSSIndex {
		return 0
	}

	rss, err := strconv.ParseInt(fields[statmRSSIndex], 10, 64)
	if err != nil {
		return 0
	}

	return rss
}
