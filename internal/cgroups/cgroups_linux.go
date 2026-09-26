//go:build linux

// Package cgroups manages one cgroup v2 group per container.
//
// cgroup v2 is a single tree of directories under /sys/fs/cgroup. Each
// directory is a group, and its files are the knobs: writing "67108864" to
// memory.max caps the group's memory, writing a PID to cgroup.procs moves a
// process in. A controller such as memory only appears in a group if its
// parent lists it in cgroup.subtree_control, so controllers are switched on
// top-down. Containers live at /sys/fs/cgroup/mycrt/<container id>.
package cgroups

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	mountPoint = "/sys/fs/cgroup"
	parentName = "mycrt"
	cpuPeriod  = 100000 // microseconds; cpu.max quotas are per period
)

var controllers = []string{"cpu", "memory", "pids"}

// Limits caps one container's resources. Zero means unlimited.
type Limits struct {
	MemoryBytes int64   // memory.max
	CPUs        float64 // share of CPUs, e.g. 0.5 for half of one CPU
	Pids        int64   // pids.max, the most processes and threads at once
}

// Cgroup is one container's group.
type Cgroup struct {
	Path string
}

// Parent returns the directory holding every container's cgroup.
func Parent() string { return filepath.Join(mountPoint, parentName) }

// Create makes the container's cgroup and applies its limits.
func Create(id string, l Limits) (*Cgroup, error) {
	if err := ensureParent(); err != nil {
		return nil, err
	}
	cg := &Cgroup{Path: filepath.Join(Parent(), id)}
	if err := os.Mkdir(cg.Path, 0o755); err != nil {
		return nil, fmt.Errorf("create cgroup: %w", err)
	}
	if err := cg.apply(l); err != nil {
		cg.Destroy()
		return nil, err
	}
	return cg, nil
}

// ensureParent creates the mycrt group and turns on the controllers
// containers need, first in the root and then in mycrt itself.
func ensureParent() error {
	var st unix.Statfs_t
	if err := unix.Statfs(mountPoint, &st); err != nil || st.Type != unix.CGROUP2_SUPER_MAGIC {
		return fmt.Errorf("%s is not a cgroup v2 mount; mycrt needs the unified hierarchy", mountPoint)
	}
	if err := os.Mkdir(Parent(), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create %s: %w", Parent(), err)
	}
	for _, dir := range []string{mountPoint, Parent()} {
		if err := enableControllers(dir); err != nil {
			return err
		}
	}
	return nil
}

func enableControllers(dir string) error {
	current, err := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		return err
	}
	enabled := strings.Fields(string(current))
	var add []string
	for _, c := range controllers {
		if !contains(enabled, c) {
			add = append(add, "+"+c)
		}
	}
	if len(add) == 0 {
		return nil
	}
	if err := write(dir, "cgroup.subtree_control", strings.Join(add, " ")); err != nil {
		return fmt.Errorf("enable controllers in %s: %w", dir, err)
	}
	return nil
}

func (c *Cgroup) apply(l Limits) error {
	if l.MemoryBytes > 0 {
		if err := write(c.Path, "memory.max", strconv.FormatInt(l.MemoryBytes, 10)); err != nil {
			return err
		}
		// Without this, a container over its limit swaps instead of
		// being stopped, and the limit only slows it down.
		if exists(c.Path, "memory.swap.max") {
			if err := write(c.Path, "memory.swap.max", "0"); err != nil {
				return err
			}
		}
	}
	// When the OOM killer fires, take down the whole container rather
	// than one process, leaving the rest in an unknown state.
	if err := write(c.Path, "memory.oom.group", "1"); err != nil {
		return err
	}
	if l.CPUs > 0 {
		quota := int64(l.CPUs * cpuPeriod)
		if quota < 1000 {
			quota = 1000 // the kernel's minimum quota is 1ms
		}
		if err := write(c.Path, "cpu.max", fmt.Sprintf("%d %d", quota, cpuPeriod)); err != nil {
			return err
		}
	}
	if l.Pids > 0 {
		if err := write(c.Path, "pids.max", strconv.FormatInt(l.Pids, 10)); err != nil {
			return err
		}
	}
	return nil
}

// Open returns the cgroup directory, for clone3's CLONE_INTO_CGROUP.
func (c *Cgroup) Open() (*os.File, error) {
	return os.OpenFile(c.Path, os.O_RDONLY|unix.O_DIRECTORY, 0)
}

// Stats is a snapshot of the group's usage counters.
type Stats struct {
	MemoryCurrent int64
	MemoryPeak    int64
	OOMKills      int64 // from memory.events
	PidsCurrent   int64
	PidsLimitHits int64 // from pids.events: forks refused at pids.max
	CPUUsageUsec  int64
	NrThrottled   int64 // periods in which the group hit its CPU quota
}

// Stats reads the group's counters. Missing files read as zero.
func (c *Cgroup) Stats() Stats {
	mem := readKeyed(c.Path, "memory.events")
	pids := readKeyed(c.Path, "pids.events")
	cpu := readKeyed(c.Path, "cpu.stat")
	return Stats{
		MemoryCurrent: readInt(c.Path, "memory.current"),
		MemoryPeak:    readInt(c.Path, "memory.peak"),
		OOMKills:      mem["oom_kill"],
		PidsCurrent:   readInt(c.Path, "pids.current"),
		PidsLimitHits: pids["max"],
		CPUUsageUsec:  cpu["usage_usec"],
		NrThrottled:   cpu["nr_throttled"],
	}
}

// Destroy kills anything still in the group and removes it.
func (c *Cgroup) Destroy() error {
	// cgroup.kill SIGKILLs every process in the group in one step. It is
	// the reliable way to stop a container that keeps forking.
	write(c.Path, "cgroup.kill", "1")
	// rmdir fails with EBUSY until the killed processes have fully exited.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := unix.Rmdir(c.Path)
		if err == nil || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if !errors.Is(err, unix.EBUSY) || time.Now().After(deadline) {
			return fmt.Errorf("remove cgroup %s: %w", c.Path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func write(dir, file, value string) error {
	if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0); err != nil {
		return fmt.Errorf("write %s: %w", file, err)
	}
	return nil
}

func exists(dir, file string) bool {
	_, err := os.Stat(filepath.Join(dir, file))
	return err == nil
}

func readInt(dir, file string) int64 {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// readKeyed parses "key value" lines, the format of *.events and *.stat.
func readKeyed(dir, file string) map[string]int64 {
	out := map[string]int64{}
	f, err := os.Open(filepath.Join(dir, file))
	if err != nil {
		return out
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if k, v, ok := strings.Cut(s.Text(), " "); ok {
			out[k], _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
