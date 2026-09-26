// Command mycontrol is the control plane. It stores pods in bbolt and runs
// the reconcile loop that keeps them running on the cluster's nodes.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"container-runtime/internal/orch/controlplane"
)

func main() {
	listen := flag.String("listen", ":7070", "address for the API")
	dbPath := flag.String("db", "/var/lib/mycontrol/state.db", "bbolt database file")
	cfg := controlplane.DefaultConfig()
	flag.DurationVar(&cfg.LeaseTimeout, "lease", cfg.LeaseTimeout, "declare a node dead after this long without a heartbeat")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("mycontrol: ")
	cfg.StartupGrace = cfg.LeaseTimeout + 2*time.Second

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		log.Fatal(err)
	}
	s, err := controlplane.Open(*dbPath, cfg, controlplane.NewHTTPAgents())
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *listen, Handler: s.Handler()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	go logEvents(ctx, s)
	log.Printf("serving on %s, state in %s, node lease %s", *listen, *dbPath, cfg.LeaseTimeout)
	s.Run(ctx)
	srv.Shutdown(context.Background())
}

// logEvents copies the activity log to stderr, e.g. for journalctl.
func logEvents(ctx context.Context, s *controlplane.Server) {
	var seq int64
	for {
		for _, e := range s.Events(seq) {
			seq = e.Seq
			log.Printf("[%s] %s", e.Pod, e.Message)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
