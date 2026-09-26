//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"container-runtime/internal/image"
	"container-runtime/internal/paths"
	"container-runtime/internal/registry"
)

func cmdPull(args []string) int {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: mycrt pull IMAGE") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if !requireRoot() {
		return 125
	}
	ref, err := image.ParseReference(fs.Arg(0))
	if err != nil {
		return fail(125, "%v", err)
	}
	store, err := image.NewStore(paths.Images())
	if err != nil {
		return fail(125, "%v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	img, err := store.Pull(ctx, registry.NewFromEnv(), ref, os.Stderr)
	if err != nil {
		return fail(125, "%v", err)
	}
	fmt.Println(img.Record.Digest)
	return 0
}

func cmdImages(args []string) int {
	if !requireRoot() {
		return 125
	}
	store, err := image.NewStore(paths.Images())
	if err != nil {
		return fail(125, "%v", err)
	}
	imgs, err := store.List()
	if err != nil {
		return fail(125, "%v", err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "IMAGE\tDIGEST\tLAYERS\tSIZE\tPULLED")
	for _, img := range imgs {
		ref, _ := image.ParseReference(img.Name)
		fmt.Fprintf(w, "%s\t%s\t%d\t%.1f MB\t%s ago\n", ref.Short(),
			strings.TrimPrefix(img.Record.Digest, "sha256:")[:12], len(img.LayerDirs),
			float64(img.CompressedSize)/(1<<20), time.Since(img.Record.PulledAt).Round(time.Second))
	}
	w.Flush()
	return 0
}
