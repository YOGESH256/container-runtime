//go:build linux

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/YOGESH256/container-runtime/internal/cgroups"
	"github.com/YOGESH256/container-runtime/internal/container"
	"github.com/YOGESH256/container-runtime/internal/engine"
)

const execEnv = container.ExecEnv

func execStage(stage string) int { return container.ExecMain(stage) }

func cmdExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: mycrt exec [-u USER] [-w DIR] [-e KEY=VALUE] CONTAINER COMMAND [ARG...]")
		fs.PrintDefaults()
	}
	user := fs.String("u", "", "user to run as (default: the container's user)")
	workdir := fs.String("w", "", "working directory (default: the container's)")
	var env multiFlag
	fs.Var(&env, "e", "set an environment variable KEY=VALUE (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return 2
	}
	if !requireRoot() {
		return 125
	}
	st, err := engine.Resolve(fs.Arg(0))
	if err != nil {
		return fail(1, "%v", err)
	}
	if st.Status != engine.StatusRunning {
		return fail(1, "container %s is not running", st.ID)
	}
	spec, err := engine.LoadSpec(st.ID)
	if err != nil {
		return fail(125, "%v", err)
	}
	cfg := container.ExecConfig{
		Pid:        st.Pid,
		CgroupPath: filepath.Join(cgroups.Parent(), st.ID),
		Args:       fs.Args()[1:],
		Env:        mergeEnv(spec.Env, env),
		Cwd:        spec.Cwd,
		User:       spec.User,
		IDShift:    engine.IDShift(),
	}
	if *user != "" {
		cfg.User = *user
	}
	if *workdir != "" {
		cfg.Cwd = *workdir
	}
	code, err := container.Exec(cfg, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return fail(125, "%v", err)
	}
	return code
}
