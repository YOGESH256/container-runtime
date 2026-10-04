//go:build linux

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/YOGESH256/container-runtime/internal/engine"
	"github.com/YOGESH256/container-runtime/internal/paths"
)

// shimEnv marks a re-executed mycrt that supervises one detached container.
const shimEnv = "_MYCRT_SHIM"

// startDetached launches the container's supervisor in its own session,
// so it outlives this command, then waits until the container is running.
func startDetached(id string) int {
	cmd := exec.Command("/proc/self/exe")
	cmd.Args = []string{"mycrt-shim", id}
	cmd.Env = append(os.Environ(), shimEnv+"="+id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fail(125, "start supervisor: %v", err)
	}
	cmd.Process.Release()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st, err := engine.Load(id)
		switch {
		case err != nil:
			// Removed already: it ran with --rm and has finished.
			fmt.Println(id)
			return 0
		case st.Status == engine.StatusRunning:
			fmt.Println(id)
			return 0
		case st.Status == engine.StatusExited && st.Error != "":
			return fail(125, "%s", st.Error)
		case st.Status == engine.StatusExited:
			fmt.Println(id)
			return 0
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fail(125, "container %s did not start within 60s", id)
}

// runShim is the supervisor process of a detached container.
func runShim(id string) int {
	logFile, err := os.OpenFile(filepath.Join(paths.Container(id), "container.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 125
	}
	defer logFile.Close()
	devNull, _ := os.Open(os.DevNull)
	code, err := engine.Run(id, engine.Stdio{In: devNull, Out: logFile, Err: logFile}, nil)
	if err != nil {
		fmt.Fprintf(logFile, "mycrt: %v\n", err)
	}
	return code
}

func cmdPs(args []string) int {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	all := fs.Bool("a", false, "include stopped containers")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !requireRoot() {
		return 125
	}
	list, err := engine.List()
	if err != nil {
		return fail(125, "%v", err)
	}
	shown := []*engine.State{}
	for _, st := range list {
		if *all || st.Status == engine.StatusRunning {
			shown = append(shown, st)
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(shown)
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "CONTAINER ID\tNAME\tIMAGE\tSTATUS\tIP\tPORTS")
	for _, st := range shown {
		var ports []string
		if spec, err := engine.LoadSpec(st.ID); err == nil {
			for _, p := range spec.Ports {
				ports = append(ports, p.String())
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", st.ID, st.Name, st.Image, describe(st), st.IP, strings.Join(ports, ","))
	}
	w.Flush()
	return 0
}

func describe(st *engine.State) string {
	switch st.Status {
	case engine.StatusRunning:
		return "Up " + ago(st.StartedAt)
	case engine.StatusExited:
		s := fmt.Sprintf("Exited (%d) %s ago", st.ExitCode, ago(st.FinishedAt))
		if st.OOMKilled {
			s += ", OOM-killed"
		}
		return s
	}
	return "Created"
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	timeout := fs.Int("t", 10, "seconds to wait for a graceful stop before killing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !requireRoot() {
		return 125
	}
	return forEach(fs.Args(), func(st *engine.State) error {
		return engine.Stop(st.ID, time.Duration(*timeout)*time.Second)
	})
}

func cmdRm(args []string) int {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	force := fs.Bool("f", false, "kill the container first if it is running")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !requireRoot() {
		return 125
	}
	return forEach(fs.Args(), func(st *engine.State) error { return engine.Remove(st.ID, *force) })
}

func forEach(refs []string, fn func(*engine.State) error) int {
	if len(refs) == 0 {
		return fail(2, "give at least one container name or ID")
	}
	code := 0
	for _, ref := range refs {
		st, err := engine.Resolve(ref)
		if err == nil {
			err = fn(st)
		}
		if err != nil {
			code = fail(1, "%v", err)
			continue
		}
		fmt.Println(ref)
	}
	return code
}

func cmdLogs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "keep printing new output until the container exits")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		return fail(2, "usage: mycrt logs [-f] CONTAINER")
	}
	if !requireRoot() {
		return 125
	}
	st, err := engine.Resolve(fs.Arg(0))
	if err != nil {
		return fail(1, "%v", err)
	}
	f, err := os.Open(filepath.Join(paths.Container(st.ID), "container.log"))
	if err != nil {
		return fail(1, "no logs: %v", err)
	}
	defer f.Close()
	for {
		io.Copy(os.Stdout, f)
		if !*follow {
			return 0
		}
		if cur, err := engine.Load(st.ID); err != nil || cur.Status != engine.StatusRunning {
			io.Copy(os.Stdout, f)
			return 0
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func cmdInspect(args []string) int {
	if len(args) != 1 {
		return fail(2, "usage: mycrt inspect CONTAINER")
	}
	if !requireRoot() {
		return 125
	}
	st, err := engine.Resolve(args[0])
	if err != nil {
		return fail(1, "%v", err)
	}
	spec, _ := engine.LoadSpec(st.ID)
	out, _ := json.MarshalIndent(struct {
		*engine.State
		Spec *engine.Spec `json:"spec,omitempty"`
	}{st, spec}, "", "  ")
	fmt.Println(strings.TrimSpace(string(out)))
	return 0
}
