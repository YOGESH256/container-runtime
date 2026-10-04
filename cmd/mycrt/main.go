//go:build linux

// Command mycrt runs OCI container images using Linux namespaces,
// cgroups v2 and overlayfs directly, with no Docker or runc underneath.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/YOGESH256/container-runtime/internal/container"
)

const usageText = `usage: mycrt <command> [flags]

commands:
  run       run a command in a new container (-d to run it in the background)
  ps        list containers
  stop      stop running containers
  rm        remove containers
  logs      print a detached container's output
  inspect   print a container's state as JSON
  pull      download an image from its registry
  images    list pulled images
`

func main() {
	// mycrt re-executes itself to become each container's init. That copy
	// is already inside the new namespaces and must not parse the CLI.
	if container.IsInitProcess() {
		container.InitMain()
		return
	}
	// A detached container's supervisor is also a re-executed mycrt.
	if id := os.Getenv(shimEnv); id != "" {
		os.Exit(runShim(id))
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "run":
		code = cmdRun(os.Args[2:])
	case "ps":
		code = cmdPs(os.Args[2:])
	case "stop":
		code = cmdStop(os.Args[2:])
	case "rm":
		code = cmdRm(os.Args[2:])
	case "logs":
		code = cmdLogs(os.Args[2:])
	case "inspect":
		code = cmdInspect(os.Args[2:])
	case "pull":
		code = cmdPull(os.Args[2:])
	case "images":
		code = cmdImages(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "mycrt: unknown command %q\n\n%s", os.Args[1], usageText)
		code = 2
	}
	os.Exit(code)
}

func requireRoot() bool {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "mycrt: must run as root (namespaces, mounts and cgroups need it)")
		return false
	}
	return true
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// multiFlag collects a repeatable string flag such as -e.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// mergeEnv returns base with every KEY=VALUE in overrides applied on top.
func mergeEnv(base []string, overrides ...[]string) []string {
	out := append([]string(nil), base...)
	for _, list := range overrides {
		for _, kv := range list {
			key, _, _ := strings.Cut(kv, "=")
			replaced := false
			for i, existing := range out {
				if k, _, _ := strings.Cut(existing, "="); k == key {
					out[i], replaced = kv, true
				}
			}
			if !replaced {
				out = append(out, kv)
			}
		}
	}
	return out
}
