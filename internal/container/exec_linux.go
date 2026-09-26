//go:build linux

package container

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// execCommand replaces the init process with the user's command, which
// then runs as PID 1. It only returns on failure.
func execCommand(cfg *Config) (int, error) {
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
	// Go's syscall versions change credentials on every thread at once.
	if err := syscall.Setgroups(u.groups); err != nil {
		return 125, fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(u.gid); err != nil {
		return 125, fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(u.uid); err != nil {
		return 125, fmt.Errorf("setuid: %w", err)
	}
	unix.Umask(0o022)
	err = unix.Exec(path, cfg.Args, env)
	if errors.Is(err, unix.ENOENT) {
		return 127, fmt.Errorf("exec %s: %w", path, err)
	}
	return 126, fmt.Errorf("exec %s: %w", path, err)
}

// lookPath finds an executable using the container's PATH, not ours.
func lookPath(file string, env []string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil // execve will report any problem
	}
	for _, dir := range filepath.SplitList(envValue(env, "PATH")) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, file)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: executable file not found in $PATH", file)
}

func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

func withDefaultHome(env []string, home string) []string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			return env
		}
	}
	return append(append([]string(nil), env...), "HOME="+home)
}

type execUser struct {
	uid, gid int
	groups   []int
	home     string
}

// resolveUser turns "", "name", "uid", "name:group" or "uid:gid" into ids
// using the container's own /etc/passwd and /etc/group. It must run after
// pivot_root so it reads the image's files, not the host's.
func resolveUser(spec string) (*execUser, error) {
	if spec == "" {
		spec = "0"
	}
	userPart, groupPart, _ := strings.Cut(spec, ":")
	passwd := readColonFile("/etc/passwd")
	u := &execUser{home: "/"}

	var name string
	if id, err := strconv.Atoi(userPart); err == nil {
		u.uid = id
		for _, f := range passwd {
			if len(f) >= 6 && f[2] == userPart {
				name, u.gid, u.home = f[0], atoi(f[3]), f[5]
				break
			}
		}
	} else {
		found := false
		for _, f := range passwd {
			if len(f) >= 6 && f[0] == userPart {
				name, u.uid, u.gid, u.home, found = f[0], atoi(f[2]), atoi(f[3]), f[5], true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("user %q not found in /etc/passwd", userPart)
		}
	}

	groups := readColonFile("/etc/group")
	if groupPart != "" {
		if id, err := strconv.Atoi(groupPart); err == nil {
			u.gid = id
		} else {
			found := false
			for _, f := range groups {
				if len(f) >= 3 && f[0] == groupPart {
					u.gid, found = atoi(f[2]), true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("group %q not found in /etc/group", groupPart)
			}
		}
	}
	// Supplementary groups: every group that lists the user as a member.
	if name != "" {
		for _, f := range groups {
			if len(f) >= 4 {
				for _, m := range strings.Split(f[3], ",") {
					if m == name {
						u.groups = append(u.groups, atoi(f[2]))
					}
				}
			}
		}
	}
	return u, nil
}

func readColonFile(path string) [][]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][]string
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Split(line, ":"))
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
