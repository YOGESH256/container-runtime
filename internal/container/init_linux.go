//go:build linux

package container

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"github.com/YOGESH256/container-runtime/internal/debug"

	"golang.org/x/sys/unix"
)

// IsInitProcess reports whether this process is the re-executed container
// init. main() must check this before doing anything else.
func IsInitProcess() bool { return os.Getenv(initEnv) == "1" }

// InitMain runs inside the new namespaces and never returns. It either
// execs the container's command or exits with a runtime error code:
// 125 for setup failures, 126 if the command can't run, 127 if not found.
func InitMain() {
	// unshare, mount, sethostname and execve all act on the calling OS
	// thread's view of namespaces. Pin this goroutine so every one of them
	// happens on the same thread, the one that finally calls execve.
	runtime.LockOSThread()
	setProcessName("mycrt-init")

	cfg, err := readConfig()
	if err != nil {
		initFail(125, "read config: %v", err)
	}
	debug.Enabled = cfg.Debug
	debug.Logf("init", "got config")
	if debug.Enabled {
		if b, err := os.ReadFile("/proc/self/status"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "Cap") || strings.HasPrefix(line, "Uid") || strings.HasPrefix(line, "NoNewPrivs") || strings.HasPrefix(line, "Seccomp:") {
					debug.Logf("init", "%s", line)
				}
			}
		}
	}
	if err := setupContainer(cfg); err != nil {
		initFail(125, "%v", err)
	}
	debug.Logf("init", "rootfs ready, exec %s", cfg.Args[0])
	if cfg.Init {
		os.Exit(runMiniInit(cfg))
	}
	code, err := execCommand(cfg)
	initFail(code, "%v", err)
}

func readConfig() (*Config, error) {
	f := os.NewFile(configFD, "config")
	defer f.Close()
	data, err := io.ReadAll(f) // blocks until the parent finishes setup
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("parent closed the config pipe")
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if len(cfg.Args) == 0 {
		return nil, fmt.Errorf("no command given")
	}
	return &cfg, nil
}

func setupContainer(cfg *Config) error {
	if cfg.CgroupNS {
		// We are already inside the container's cgroup, so a cgroup
		// namespace created now is rooted there.
		if err := unix.Unshare(unix.CLONE_NEWCGROUP); err != nil {
			return fmt.Errorf("unshare cgroup namespace: %w", err)
		}
	}
	if err := setupRootfs(cfg); err != nil {
		return err
	}
	debug.Logf("init", "mounts done")
	if err := unix.Sethostname([]byte(cfg.Hostname)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	// A new network namespace starts with only a loopback device, down.
	if err := setLinkUp("lo"); err != nil {
		return fmt.Errorf("bring up lo: %w", err)
	}
	if err := pivotRoot(cfg.Rootfs); err != nil {
		return err
	}
	if cfg.Terminal {
		return attachPTY()
	}
	return nil
}

func setLinkUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

func setProcessName(name string) {
	b := append([]byte(name), 0)
	unix.Prctl(unix.PR_SET_NAME, uintptr(unsafe.Pointer(&b[0])), 0, 0, 0)
}

func initFail(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mycrt: "+format+"\n", args...)
	os.Exit(code)
}
