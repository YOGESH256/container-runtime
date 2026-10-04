//go:build linux

// Package engine runs containers and tracks their lifecycle. It prepares
// a container's directory and overlay layers, creates its cgroup and
// network, starts it through package container, records its state, and
// releases everything when it exits.
package engine

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/YOGESH256/container-runtime/internal/cgroups"
	"github.com/YOGESH256/container-runtime/internal/container"
	"github.com/YOGESH256/container-runtime/internal/debug"
	"github.com/YOGESH256/container-runtime/internal/network"
	"github.com/YOGESH256/container-runtime/internal/paths"
	"github.com/YOGESH256/container-runtime/internal/units"
)

// IDShift is the host user ID that root in a container maps to; the
// container owns the IDShift..IDShift+65535 range. MYCRT_UID_SHIFT
// overrides it. Images are unpacked with this shift, so changing it means
// re-pulling.
func IDShift() int {
	if v := os.Getenv("MYCRT_UID_SHIFT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 100000
}

// Spec is everything needed to create one container.
type Spec struct {
	ID         string                `json:"id"`
	Image      string                `json:"image"`     // shown to users, e.g. "alpine:3.20"
	Lowerdirs  []string              `json:"lowerdirs"` // read-only layers, top layer first
	Args       []string              `json:"args"`
	Env        []string              `json:"env"`
	Cwd        string                `json:"cwd,omitempty"`
	User       string                `json:"user,omitempty"`
	Hostname   string                `json:"hostname"`
	Init       bool                  `json:"init,omitempty"`
	Limits     cgroups.Limits        `json:"limits"`
	StopSignal syscall.Signal        `json:"stopSignal,omitempty"`
	Network    string                `json:"network"` // "bridge" (default) or "none"
	Ports      []network.PortMapping `json:"ports,omitempty"`
}

// Stdio is the container's standard input, output and error.
type Stdio struct {
	In, Out, Err *os.File
	// Terminal gives the container its own pseudo-terminal, relayed to In
	// and Out.
	Terminal bool
}

// Run starts a container registered with Create and supervises it until
// it exits. The calling process is the container's supervisor: signals it
// receives are relayed, and it records the outcome in the state file.
// It returns the exit code of the container's PID 1.
//
// sigs carries signals to relay to the container. Callers should subscribe
// before creating anything (container.SubscribeSignals), so a stop request
// during setup is queued rather than killing mycrt halfway. If nil, Run
// subscribes itself.
func Run(id string, stdio Stdio, sigs chan os.Signal) (int, error) {
	if sigs == nil {
		sigs = container.SubscribeSignals()
		defer signal.Stop(sigs)
	}
	st, err := Load(id)
	if err != nil {
		return -1, err
	}
	spec, err := LoadSpec(id)
	if err != nil {
		return -1, err
	}
	code, runErr := run(spec, st, stdio, sigs)
	st.Status, st.ExitCode, st.FinishedAt = StatusExited, code, time.Now().UTC()
	if runErr != nil {
		st.Error, st.ExitCode = runErr.Error(), 125
	}
	if st.AutoRemove {
		os.RemoveAll(paths.Container(id))
	} else if err := st.save(); err != nil && runErr == nil {
		runErr = err
	}
	return st.ExitCode, runErr
}

func run(spec *Spec, st *State, stdio Stdio, sigs <-chan os.Signal) (int, error) {
	shift := IDShift()
	dir := paths.Container(spec.ID)
	// The container's root (host user shift) mounts the overlay itself,
	// so it needs to own the upper, work and mount directories and to
	// traverse the ones above them.
	if err := os.Chmod(dir, 0o711); err != nil {
		return -1, err
	}
	upper, work, merged := filepath.Join(dir, "upper"), filepath.Join(dir, "work"), filepath.Join(dir, "rootfs")
	for _, d := range []string{upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return -1, err
		}
		if err := os.Chown(d, shift, shift); err != nil {
			return -1, err
		}
	}
	lowerdirs := spec.Lowerdirs
	if len(lowerdirs) == 0 {
		// overlayfs needs at least one lower layer, even for an empty image.
		empty := filepath.Join(dir, "empty")
		if err := os.MkdirAll(empty, 0o755); err != nil {
			return -1, err
		}
		os.Chown(empty, shift, shift)
		lowerdirs = []string{empty}
	}

	debug.Logf("engine", "dirs ready")
	cg, err := cgroups.Create(spec.ID, spec.Limits)
	if err != nil {
		return -1, err
	}
	debug.Logf("engine", "cgroup created")
	defer func() {
		if err := cg.Destroy(); err != nil {
			fmt.Fprintf(stdio.Err, "mycrt: %v\n", err)
		}
		debug.Logf("engine", "cgroup removed")
	}()
	cgDir, err := cg.Open()
	if err != nil {
		return -1, fmt.Errorf("open cgroup: %w", err)
	}
	defer cgDir.Close()
	if err := cgroups.AttachDeviceFilter(cgDir); err != nil {
		return -1, err
	}

	// Networking: lease an address now, wire the veth pair once the
	// container's network namespace exists (the Setup hook below).
	var netw *network.Network
	var ip net.IP
	if spec.Network != "none" {
		if netw, err = network.Default(); err != nil {
			return -1, err
		}
		if err := netw.EnsureHost(); err != nil {
			return -1, err
		}
		debug.Logf("engine", "bridge and NAT ready")
		if ip, err = netw.Allocate(spec.ID); err != nil {
			return -1, err
		}
		if len(spec.Ports) > 0 {
			if err := checkPortConflicts(spec); err != nil {
				netw.Release(spec.ID)
				return -1, err
			}
			if err := netw.PublishPorts(spec.ID, ip, spec.Ports); err != nil {
				netw.Release(spec.ID)
				return -1, err
			}
			defer netw.UnpublishPorts(spec.ID)
		}
		debug.Logf("engine", "leased %s", ip)
		// No explicit veth deletion: the kernel removes the pair when the
		// container's network namespace dies, without making us wait.
		defer func() {
			netw.Release(spec.ID)
			debug.Logf("engine", "lease released")
		}()
	}
	files, err := network.EtcFiles(dir, spec.Hostname, ip)
	if err != nil {
		return -1, err
	}

	code, err := container.Run(&container.Process{
		Config: container.Config{
			ID:        spec.ID,
			Hostname:  spec.Hostname,
			Args:      spec.Args,
			Env:       spec.Env,
			Cwd:       spec.Cwd,
			User:      spec.User,
			Init:      spec.Init,
			Rootfs:    merged,
			Lowerdirs: lowerdirs,
			Upperdir:  upper,
			Workdir:   work,
			Files:     files,
			CgroupNS:  true,
			Debug:     debug.Enabled,
		},
		Stdin:    stdio.In,
		Stdout:   stdio.Out,
		Stderr:   stdio.Err,
		CgroupFD: int(cgDir.Fd()),
		IDShift:  shift,
		Terminal: stdio.Terminal,
		Setup: func(pid int) error {
			if netw == nil {
				return nil
			}
			err := netw.Attach(spec.ID, pid, ip)
			debug.Logf("engine", "veth attached")
			return err
		},
		Started: func(pid int) {
			st.Status, st.Pid, st.ShimPid, st.StartedAt = StatusRunning, pid, os.Getpid(), time.Now().UTC()
			st.PidStart, _ = procStartTime(pid)
			if ip != nil {
				st.IP = ip.String()
			}
			st.save()
			debug.Logf("engine", "container released, pid %d", pid)
		},
		Signals:    sigs,
		StopSignal: spec.StopSignal,
	})
	debug.Logf("engine", "container exited with %d", code)
	if err != nil {
		return -1, err
	}
	if stats := cg.Stats(); stats.OOMKills > 0 {
		st.OOMKilled = true
		fmt.Fprintf(stdio.Err, "mycrt: container was OOM-killed: it hit its %s memory limit\n",
			units.FormatBytes(spec.Limits.MemoryBytes))
	}
	return code, nil
}

// checkPortConflicts refuses a host port another running container holds.
func checkPortConflicts(spec *Spec) error {
	all, err := List()
	if err != nil {
		return err
	}
	for _, st := range all {
		if st.ID == spec.ID || st.Status != StatusRunning {
			continue
		}
		other, err := LoadSpec(st.ID)
		if err != nil {
			continue
		}
		for _, a := range spec.Ports {
			for _, b := range other.Ports {
				if a.Conflicts(b) {
					return fmt.Errorf("host port %d/%s is already published by container %s", a.HostPort, a.Proto, st.ID)
				}
			}
		}
	}
	return nil
}
