// Command myagent is the node agent. It runs pod containers on this node
// with mycrt, probes their health, and reports to the control plane.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"container-runtime/internal/orch/agent"
)

func main() {
	hostname, _ := os.Hostname()
	exe, _ := os.Executable()
	name := flag.String("name", hostname, "this node's name")
	controlPlane := flag.String("control-plane", "", "control plane URL, e.g. http://192.168.104.1:7070 (required)")
	listen := flag.String("listen", ":7071", "address for the agent API")
	advertise := flag.String("advertise", "", "URL the control plane uses to reach this agent (default: detected)")
	mycrt := flag.String("mycrt", filepath.Join(filepath.Dir(exe), "mycrt"), "path to the mycrt binary")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("myagent: ")
	if *controlPlane == "" {
		log.Fatal("--control-plane is required")
	}
	if os.Geteuid() != 0 {
		log.Fatal("must run as root, because it runs mycrt")
	}
	if *advertise == "" {
		_, port, err := net.SplitHostPort(*listen)
		if err != nil {
			log.Fatalf("--listen: %v", err)
		}
		if *advertise, err = agent.AdvertiseAddress(*controlPlane, port); err != nil {
			log.Fatalf("cannot work out an address to advertise: %v (set --advertise)", err)
		}
	}

	a := agent.New(agent.Config{Node: *name, ControlPlane: *controlPlane, Advertise: *advertise}, agent.Mycrt{Path: *mycrt})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *listen, Handler: a.Handler()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	go a.RunPrepull(ctx)
	log.Printf("node %s serving on %s, advertising %s, reporting to %s", *name, *listen, *advertise, *controlPlane)
	a.RunHeartbeats(ctx)
	// Containers keep running: each has its own mycrt supervisor.
	srv.Shutdown(context.Background())
	log.Print("stopped; containers left running")
}
