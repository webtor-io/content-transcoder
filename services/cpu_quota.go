package services

import (
	"math"
	"os"
	"strconv"
	"strings"
)

// cpuQuotaThreads is the CPU the container may use, rounded up, as a thread
// count for FFmpeg, or 0 when there is no quota (FFmpeg then sizes its
// pools from the visible cores, which is right without a limit -- the
// self-hosted image runs without one). cgroup v2 first, then v1.
func cpuQuotaThreads() int {
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		return threadsFromCPUMax(string(b))
	}
	q, err1 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	p, err2 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err1 != nil || err2 != nil {
		return 0
	}
	return threadsFromCPUMax(strings.TrimSpace(string(q)) + " " + strings.TrimSpace(string(p)))
}

// threadsFromCPUMax reads a cgroup v2 cpu.max line ("<quota> <period>" or
// "max <period>"; v1's quota is -1 for none).
func threadsFromCPUMax(s string) int {
	f := strings.Fields(s)
	if len(f) != 2 || f[0] == "max" {
		return 0
	}
	quota, err1 := strconv.ParseFloat(f[0], 64)
	period, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return int(math.Ceil(quota / period))
}
