//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"container-runtime/internal/cgroups"
	"container-runtime/internal/container"
	"container-runtime/internal/engine"
	"container-runtime/internal/image"
	"container-runtime/internal/paths"
	"container-runtime/internal/registry"
	"container-runtime/internal/units"

	"golang.org/x/sys/unix"
)

const defaultPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: mycrt run [flags] IMAGE [COMMAND [ARG...]]\n"+
			"       mycrt run [flags] --rootfs DIR COMMAND [ARG...]\n\nflags:\n")
		fs.PrintDefaults()
	}
	rootfs := fs.String("rootfs", "", "run from an unpacked root filesystem directory instead of an image")
	entrypoint := fs.String("entrypoint", "", "override the image's entrypoint")
	hostname := fs.String("hostname", "", "container hostname (default: the container ID)")
	useInit := fs.Bool("init", false, "run a minimal init as PID 1 that reaps zombies and relays signals")
	workdir := fs.String("w", "", "working directory inside the container")
	user := fs.String("u", "", "user to run as: name, uid, name:group or uid:gid")
	memory := fs.String("memory", "", "memory limit, e.g. 64m or 1g (default: unlimited)")
	cpus := fs.Float64("cpus", 0, "CPU limit as a number of CPUs, e.g. 0.5 (default: unlimited)")
	pids := fs.Int64("pids", 1024, "maximum number of processes and threads (0: unlimited)")
	netMode := fs.String("net", "bridge", "network: bridge (own IP on the mycrt0 bridge, NAT out) or none (loopback only)")
	detach := fs.Bool("d", false, "run in the background and print the container ID")
	name := fs.String("name", "", "give the container a unique name")
	autoRemove := fs.Bool("rm", false, "remove the container when it exits (always on without -d)")
	var labels multiFlag
	fs.Var(&labels, "label", "attach a KEY=VALUE label (repeatable)")
	var env multiFlag
	fs.Var(&env, "e", "set an environment variable KEY=VALUE (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !requireRoot() {
		return 125
	}
	// From here on, SIGTERM and friends are queued for the container
	// instead of killing mycrt in the middle of setting it up.
	sigs := container.SubscribeSignals()
	defer signal.Stop(sigs)
	flagSet := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { flagSet[f.Name] = true })

	if *netMode != "bridge" && *netMode != "none" {
		return fail(2, "--net must be bridge or none")
	}
	spec := &engine.Spec{
		ID:      newID(),
		Init:    *useInit,
		Limits:  cgroups.Limits{CPUs: *cpus, Pids: *pids},
		Network: *netMode,
	}
	if *memory != "" {
		n, err := units.ParseBytes(*memory)
		if err != nil {
			return fail(2, "--memory: %v", err)
		}
		spec.Limits.MemoryBytes = n
	}

	positional := fs.Args()
	var imageEnv []string
	if *rootfs != "" {
		if len(positional) == 0 {
			fs.Usage()
			return 2
		}
		dir, err := filepath.Abs(*rootfs)
		if err != nil {
			return fail(125, "%v", err)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return fail(125, "rootfs %s is not a directory", dir)
		}
		spec.Image, spec.Lowerdirs, spec.Args = "rootfs:"+dir, []string{dir}, positional
	} else {
		if len(positional) == 0 {
			fs.Usage()
			return 2
		}
		ref, err := image.ParseReference(positional[0])
		if err != nil {
			return fail(125, "%v", err)
		}
		img, err := getOrPull(ref)
		if err != nil {
			return fail(125, "%v", err)
		}
		c := img.Config.Config
		// Docker's rules: arguments after the image replace Cmd, and the
		// entrypoint (if any) runs with Cmd as its arguments.
		entry, cmd := c.Entrypoint, c.Cmd
		if flagSet["entrypoint"] {
			entry = nil
			if *entrypoint != "" {
				entry = []string{*entrypoint}
			}
		}
		if len(positional) > 1 {
			cmd = positional[1:]
		}
		spec.Args = append(slices.Clone(entry), cmd...)
		if len(spec.Args) == 0 {
			return fail(125, "%s has no default command; give one after the image name", ref.Short())
		}
		// overlayfs lists the top layer first; manifests list it last.
		spec.Lowerdirs = slices.Clone(img.LayerDirs)
		slices.Reverse(spec.Lowerdirs)
		spec.Image, spec.Cwd, spec.User, imageEnv = ref.Short(), c.WorkingDir, c.User, c.Env
		if c.StopSignal != "" {
			sig, err := parseSignal(c.StopSignal)
			if err != nil {
				return fail(125, "image stop signal: %v", err)
			}
			spec.StopSignal = sig
		}
	}
	if flagSet["w"] {
		spec.Cwd = *workdir
	}
	if flagSet["u"] {
		spec.User = *user
	}
	spec.Hostname = *hostname
	if spec.Hostname == "" {
		spec.Hostname = spec.ID
	}
	baseEnv := []string{defaultPath}
	if term := os.Getenv("TERM"); term != "" && isTerminal(os.Stdin) {
		baseEnv = append(baseEnv, "TERM="+term)
	}
	spec.Env = mergeEnv(baseEnv, imageEnv, []string{"HOSTNAME=" + spec.Hostname}, env)

	if *name != "" && !nameRe.MatchString(*name) {
		return fail(2, "invalid name %q: use letters, digits, '_', '.' and '-'", *name)
	}
	labelMap := map[string]string{}
	for _, kv := range labels {
		k, v, _ := strings.Cut(kv, "=")
		labelMap[k] = v
	}
	// A foreground container is removed when it exits; a detached one is
	// kept, so its state and logs can be read, until `mycrt rm`.
	st, err := engine.Create(spec, *name, labelMap, *autoRemove || !*detach)
	if err != nil {
		return fail(125, "%v", err)
	}
	if *detach {
		return startDetached(st.ID)
	}
	code, err := engine.Run(st.ID, engine.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}, sigs)
	if err != nil {
		return fail(125, "%v", err)
	}
	return code
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// getOrPull returns the local image, pulling it first if needed.
func getOrPull(ref image.Reference) (*image.Image, error) {
	store, err := image.NewStore(paths.Images())
	if err != nil {
		return nil, err
	}
	img, err := store.Get(ref)
	if err == nil || !image.IsNotFound(err) {
		return img, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return store.Pull(ctx, registry.NewFromEnv(), ref, os.Stderr)
}

// parseSignal accepts "SIGQUIT", "QUIT" or "3".
func parseSignal(s string) (syscall.Signal, error) {
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 65 {
		return syscall.Signal(n), nil
	}
	name := strings.ToUpper(s)
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	if sig := unix.SignalNum(name); sig != 0 {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

func fail(code int, format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "mycrt: "+format+"\n", args...)
	return code
}
