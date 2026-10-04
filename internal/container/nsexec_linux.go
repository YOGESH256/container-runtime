//go:build linux

package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// ExecEnv marks a re-executed mycrt that is entering a running container.
// Its value is the stage: "1" joins the namespaces, "2" runs the command.
const ExecEnv = "_MYCRT_EXEC"

const execCfgEnv = "_MYCRT_EXEC_CFG"

// ExecConfig describes a command to run inside an existing container.
type ExecConfig struct {
	Pid        int      `json:"pid"`        // host PID of the container's init
	CgroupPath string   `json:"cgroupPath"` // the container's cgroup directory
	Args       []string `json:"args"`
	Env        []string `json:"env"`
	Cwd        string   `json:"cwd,omitempty"`
	User       string   `json:"user,omitempty"`
	IDShift    int      `json:"idShift"`
}

// Exec runs cfg.Args inside the container and returns its exit code.
//
// It works in two re-executed stages, because joining a PID namespace
// only affects processes created afterwards:
//
//	stage 1  joins the cgroup and the mount, UTS, IPC, network and PID
//	         namespaces, then forks stage 2
//	stage 2  is the first process inside: it switches to the requested
//	         user, applies the container's restrictions and execs
//
// One limit: the Go runtime is multithreaded before main runs, and a
// multithreaded process may not join a user namespace. So an exec'd
// process stays outside the container's user namespace and runs as the
// shifted host user instead. Inside, `id` prints that number rather than
// 0, but file access and every restriction are the same.
func Exec(cfg ExecConfig, stdin, stdout, stderr *os.File) (int, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return -1, err
	}
	stdio, err := prepareStdio(stdin, stdout, stderr, cfg.IDShift, false)
	if err != nil {
		return -1, err
	}
	defer stdio.finish()
	cmd := exec.Command("/proc/self/exe")
	cmd.Args = []string{"mycrt-exec"}
	cmd.Env = []string{ExecEnv + "=1", execCfgEnv + "=" + string(b)}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.in, stdio.out, stdio.err
	sigs := SubscribeSignals()
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		stdio.started()
		return -1, err
	}
	stdio.started()
	done := make(chan struct{})
	go forwardSignals(sigs, done, cmd.Process.Pid, 0, isTerminal(stdin), stderr)
	err = cmd.Wait()
	close(done)
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return -1, err
	}
	return exitCode(cmd.ProcessState.Sys().(syscall.WaitStatus)), nil
}

// ExecMain runs one stage and never returns.
func ExecMain(stage string) int {
	runtime.LockOSThread()
	var cfg ExecConfig
	if err := json.Unmarshal([]byte(os.Getenv(execCfgEnv)), &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "mycrt: exec config: %v\n", err)
		return 125
	}
	switch stage {
	case "1":
		return execStage1(&cfg)
	case "2":
		code, err := execStage2(&cfg)
		fmt.Fprintf(os.Stderr, "mycrt: %v\n", err)
		return code
	}
	return 125
}

func execStage1(cfg *ExecConfig) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "mycrt: "+format+"\n", args...)
		return 125
	}
	// Join the cgroup first, while still outside everything, so the
	// command counts against the container's limits.
	if err := os.WriteFile(filepath.Join(cfg.CgroupPath, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return fail("join cgroup: %v", err)
	}
	// Open every namespace before entering any: once in the container's
	// mount namespace, the host's /proc is gone.
	kinds := []string{"ipc", "uts", "net", "pid", "mnt"}
	fds := make([]int, len(kinds))
	for i, k := range kinds {
		fd, err := unix.Open(fmt.Sprintf("/proc/%d/ns/%s", cfg.Pid, k), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fail("open %s namespace: %v (is the container running?)", k, err)
		}
		fds[i] = fd
	}
	// Go's threads share one filesystem context (root, cwd). The kernel
	// refuses to move such a thread into another mount namespace, so give
	// this thread a private copy first.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fail("unshare fs: %v", err)
	}
	for i, k := range kinds {
		if err := unix.Setns(fds[i], 0); err != nil {
			return fail("enter %s namespace: %v", k, err)
		}
		unix.Close(fds[i])
	}
	cmd := exec.Command("/proc/self/exe")
	cmd.Args = []string{"mycrt-exec"}
	cmd.Env = []string{ExecEnv + "=2", execCfgEnv + "=" + os.Getenv(execCfgEnv)}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// No Pdeathsig here: Go checks it with getppid(), which returns 0
	// inside the container's PID namespace, and would kill the child.
	sigs := SubscribeSignals()
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return fail("start: %v", err)
	}
	done := make(chan struct{})
	go forwardSignals(sigs, done, cmd.Process.Pid, 0, isTerminal(os.Stdin), os.Stderr)
	err := cmd.Wait()
	close(done)
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return fail("%v", err)
	}
	return exitCode(cmd.ProcessState.Sys().(syscall.WaitStatus))
}

// execStage2 is inside the container's namespaces (except user) with its
// root and working directory already at the container's root.
func execStage2(cfg *ExecConfig) (int, error) {
	u, err := resolveUser(cfg.User)
	if err != nil {
		return 125, err
	}
	env := withDefaultHome(cfg.Env, u.home)
	if cfg.Cwd != "" {
		if err := os.Chdir(cfg.Cwd); err != nil {
			return 125, fmt.Errorf("working directory: %w", err)
		}
	}
	path, err := lookPath(cfg.Args[0], env)
	if err != nil {
		return 127, err
	}
	// Outside the user namespace, the container's uid N is host uid
	// N+shift. No capabilities are kept: even "root" exec'd this way is
	// an ordinary user to the host.
	u.uid += cfg.IDShift
	u.gid += cfg.IDShift
	for i := range u.groups {
		u.groups[i] += cfg.IDShift
	}
	if err := hardenPre(); err != nil {
		return 125, err
	}
	if err := syscall.Setgroups(u.groups); err != nil {
		return 125, fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(u.gid); err != nil {
		return 125, fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(u.uid); err != nil {
		return 125, fmt.Errorf("setuid: %w", err)
	}
	if err := hardenPost(false); err != nil {
		return 125, err
	}
	unix.Umask(0o022)
	err = unix.Exec(path, cfg.Args, env)
	if errors.Is(err, unix.ENOENT) {
		return 127, fmt.Errorf("exec %s: %w", path, err)
	}
	return 126, fmt.Errorf("exec %s: %w", path, err)
}
