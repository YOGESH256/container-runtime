//go:build linux

package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"container-runtime/internal/cgroups"
	"container-runtime/internal/fsync"
	"container-runtime/internal/network"
	"container-runtime/internal/paths"

	"golang.org/x/sys/unix"
)

// Status is where a container is in its life.
type Status string

const (
	StatusCreated Status = "created"
	StatusRunning Status = "running"
	StatusExited  Status = "exited"
)

// State is a container's lifecycle record, kept as state.json in its
// directory. Only the process supervising the container writes it.
type State struct {
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	Image      string            `json:"image"`
	Labels     map[string]string `json:"labels,omitempty"`
	Status     Status            `json:"status"`
	Pid        int               `json:"pid,omitempty"`      // host PID of the container's init
	PidStart   uint64            `json:"pidStart,omitempty"` // its start time, to tell a reused PID apart
	ShimPid    int               `json:"shimPid,omitempty"`  // the mycrt process supervising it
	IP         string            `json:"ip,omitempty"`
	ExitCode   int               `json:"exitCode"`
	OOMKilled  bool              `json:"oomKilled,omitempty"`
	Error      string            `json:"error,omitempty"`
	AutoRemove bool              `json:"autoRemove,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
	StartedAt  time.Time         `json:"startedAt,omitzero"`
	FinishedAt time.Time         `json:"finishedAt,omitzero"`
}

// ErrNoSuchContainer is returned when a name or ID matches nothing.
var ErrNoSuchContainer = errors.New("no such container")

// Create registers a new container: its directory, spec and initial state.
// Names are unique, so the check and the creation happen under a lock.
func Create(spec *Spec, name string, labels map[string]string, autoRemove bool) (*State, error) {
	unlock, err := lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if name != "" {
		all, _ := List()
		for _, st := range all {
			if st.Name == name {
				return nil, fmt.Errorf("name %q is already used by container %s", name, st.ID)
			}
		}
	}
	dir := paths.Container(spec.ID)
	if err := os.MkdirAll(paths.Containers(), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, "spec.json"), spec); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	st := &State{ID: spec.ID, Name: name, Image: spec.Image, Labels: labels,
		Status: StatusCreated, AutoRemove: autoRemove, CreatedAt: time.Now().UTC()}
	if err := st.save(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return st, nil
}

// Load reads a container's state by exact ID.
func Load(id string) (*State, error) {
	var st State
	if err := readJSON(filepath.Join(paths.Container(id), "state.json"), &st); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNoSuchContainer, id)
		}
		return nil, err
	}
	st.refresh()
	return &st, nil
}

// LoadSpec reads the spec a container was created with.
func LoadSpec(id string) (*Spec, error) {
	var spec Spec
	return &spec, readJSON(filepath.Join(paths.Container(id), "spec.json"), &spec)
}

// List returns every container, oldest first.
func List() ([]*State, error) {
	entries, err := os.ReadDir(paths.Containers())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*State
	for _, e := range entries {
		if st, err := Load(e.Name()); err == nil {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Resolve finds a container by name, full ID or unique ID prefix.
func Resolve(ref string) (*State, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	var matches []*State
	for _, st := range all {
		if st.ID == ref || st.Name == ref {
			return st, nil
		}
		if strings.HasPrefix(st.ID, ref) {
			matches = append(matches, st)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: %s", ErrNoSuchContainer, ref)
	case 1:
		return matches[0], nil
	}
	return nil, fmt.Errorf("%q matches %d containers; use more of the ID", ref, len(matches))
}

// Stop asks the container to stop and waits up to timeout for it to exit.
// SIGTERM goes to the supervising mycrt, which relays the image's stop
// signal to PID 1. If the container outlives the grace period, every
// process in its cgroup is killed at once through cgroup.kill.
func Stop(id string, timeout time.Duration) error {
	st, err := Load(id)
	if err != nil {
		return err
	}
	// A container that is still being set up has no PID yet. Give its
	// supervisor a moment to start it, then stop it like any other.
	for deadline := time.Now().Add(30 * time.Second); st.Status == StatusCreated && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		if st, err = Load(id); err != nil {
			return nil // removed meanwhile
		}
	}
	if st.Status != StatusRunning {
		return nil
	}
	if timeout > 0 {
		syscall.Kill(st.ShimPid, syscall.SIGTERM)
		if waitExited(id, timeout) {
			return nil
		}
	}
	os.WriteFile(filepath.Join(cgroups.Parent(), id, "cgroup.kill"), []byte("1"), 0)
	if waitExited(id, 10*time.Second) {
		return nil
	}
	return fmt.Errorf("container %s did not stop", id)
}

// Remove deletes a container and anything it still holds. A running
// container is refused unless force is set, in which case it is killed.
func Remove(id string, force bool) error {
	st, err := Load(id)
	if err != nil {
		return err
	}
	if st.Status == StatusRunning {
		if !force {
			return fmt.Errorf("container %s is running; stop it first or force removal", id)
		}
		if err := Stop(id, 0); err != nil {
			return err
		}
	}
	// A supervisor that crashed may have left its cgroup, veth or lease.
	cg := &cgroups.Cgroup{Path: filepath.Join(cgroups.Parent(), id)}
	cg.Destroy()
	if n, err := network.Default(); err == nil {
		n.Detach(id)
		n.Release(id)
	}
	return os.RemoveAll(paths.Container(id))
}

func waitExited(id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := Load(id)
		if err != nil || st.Status != StatusRunning {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// refresh notices a container whose processes vanished without their
// supervisor recording it, for example after a host reboot.
func (st *State) refresh() {
	if st.Status != StatusRunning {
		return
	}
	if start, err := procStartTime(st.Pid); err == nil && start == st.PidStart {
		return
	}
	st.Status, st.ExitCode = StatusExited, -1
	st.Error = "container processes are gone (host reboot or supervisor crash)"
}

func (st *State) save() error {
	return writeJSON(filepath.Join(paths.Container(st.ID), "state.json"), st)
}

// procStartTime reads a process's start time from /proc/<pid>/stat. A PID
// can be reused by a new process; the pair (PID, start time) cannot.
func procStartTime(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The command name in field 2 may contain spaces, so parse after it.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, errors.New("malformed stat")
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 20 {
		return 0, errors.New("malformed stat")
	}
	return strconv.ParseUint(fields[19], 10, 64) // field 22 overall
}

func lock() (func(), error) {
	if err := os.MkdirAll(paths.Root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(paths.Root, "containers.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsync.WriteFile(path, b, 0o600)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
