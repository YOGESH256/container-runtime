package controlplane

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YOGESH256/container-runtime/internal/orch"
)

// fakeCluster stands in for the node agents. Run and Stop change its
// containers immediately, and a goroutine sends heartbeats for every node
// that is not paused, the way real agents would.
type fakeCluster struct {
	mu         sync.Mutex
	containers map[string]map[string]*orch.ContainerReport // node -> pod/gen -> report
	paused     map[string]bool                             // no heartbeats, containers keep running
	dead       map[string]bool                             // no heartbeats, and requests fail
	crashing   map[string]bool                             // pods whose containers exit at once
	memory     int64
}

func newFakeCluster(nodes ...string) *fakeCluster {
	f := &fakeCluster{containers: map[string]map[string]*orch.ContainerReport{},
		paused: map[string]bool{}, dead: map[string]bool{}, crashing: map[string]bool{}, memory: 1 << 30}
	for _, n := range nodes {
		f.containers[n] = map[string]*orch.ContainerReport{}
	}
	return f
}

func nodeOf(addr string) string { return strings.TrimPrefix(addr, "fake://") }

func (f *fakeCluster) Run(_ context.Context, addr string, req orch.RunRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	node := nodeOf(addr)
	if f.dead[node] {
		return errors.New("connection refused")
	}
	key := runKey(req.Pod, req.Generation)
	if _, ok := f.containers[node][key]; !ok {
		f.containers[node][key] = &orch.ContainerReport{ID: node + "-" + key, Pod: req.Pod, Generation: req.Generation,
			State: orch.StateRunning, IP: "10.88.0.2", Healthy: true}
		if f.crashing[req.Pod] {
			f.containers[node][key].State, f.containers[node][key].ExitCode = orch.StateExited, 1
		}
	}
	return nil
}

func (f *fakeCluster) Stop(_ context.Context, addr, pod string, gen int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	node := nodeOf(addr)
	if f.dead[node] {
		return errors.New("connection refused")
	}
	delete(f.containers[node], runKey(pod, gen))
	return nil
}

func (f *fakeCluster) heartbeats(ctx context.Context, s *Server) {
	for {
		f.mu.Lock()
		var hbs []orch.Heartbeat
		for node, cs := range f.containers {
			if f.paused[node] || f.dead[node] {
				continue
			}
			hb := orch.Heartbeat{Node: node, Address: "fake://" + node, Capacity: orch.Capacity{MemoryBytes: f.memory, CPUs: 2}}
			for _, c := range cs {
				hb.Containers = append(hb.Containers, *c)
			}
			hbs = append(hbs, hb)
		}
		f.mu.Unlock()
		for _, hb := range hbs {
			s.Heartbeat(hb)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Millisecond):
		}
	}
}

// where returns the nodes currently running a container for pod.
func (f *fakeCluster) where(pod string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for node, cs := range f.containers {
		for _, c := range cs {
			if c.Pod == pod {
				out = append(out, node)
			}
		}
	}
	return out
}

func (f *fakeCluster) set(fn func()) { f.mu.Lock(); fn(); f.mu.Unlock() }

func testConfig() Config {
	return Config{LeaseTimeout: 300 * time.Millisecond, ReconcileEvery: 20 * time.Millisecond,
		StartupGrace: 0, RunResend: 200 * time.Millisecond, StartTimeout: 5 * time.Second, StopResend: 100 * time.Millisecond,
		BackoffBase: 200 * time.Millisecond, BackoffMax: time.Second, StableAfter: 500 * time.Millisecond}
}

func start(t *testing.T, f *fakeCluster, dbPath string, cfg Config) (*Server, func()) {
	t.Helper()
	s, err := Open(dbPath, cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	go f.heartbeats(ctx, s)
	return s, func() { cancel(); s.Close() }
}

// eventually polls cond for up to 3 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func running(s *Server, name string) (*orch.Pod, bool) {
	p, err := s.Pod(name)
	return p, err == nil && p.Status.Phase == orch.Running
}

func apply(t *testing.T, s *Server, spec orch.PodSpec) {
	t.Helper()
	if _, err := s.Apply(spec); err != nil {
		t.Fatal(err)
	}
}

func TestSpreadsPodsAcrossNodes(t *testing.T) {
	f := newFakeCluster("n1", "n2", "n3")
	s, stop := start(t, f, filepath.Join(t.TempDir(), "db"), testConfig())
	defer stop()
	for _, name := range []string{"a", "b", "c"} {
		apply(t, s, orch.PodSpec{Name: name, Image: "nginx"})
	}
	eventually(t, "three running pods on three nodes", func() bool {
		seen := map[string]bool{}
		for _, name := range []string{"a", "b", "c"} {
			p, ok := running(s, name)
			if !ok {
				return false
			}
			seen[p.Status.Node] = true
		}
		return len(seen) == 3
	})
}

func TestDeadNodeIsReplaced(t *testing.T) {
	f := newFakeCluster("n1", "n2")
	s, stop := start(t, f, filepath.Join(t.TempDir(), "db"), testConfig())
	defer stop()
	apply(t, s, orch.PodSpec{Name: "web", Image: "nginx"})
	var first *orch.Pod
	eventually(t, "web running", func() bool { var ok bool; first, ok = running(s, "web"); return ok })

	f.set(func() { f.dead[first.Status.Node] = true })
	killed := time.Now()
	eventually(t, "web running on the other node", func() bool {
		p, ok := running(s, "web")
		return ok && p.Status.Node != first.Status.Node && p.Status.Generation == first.Status.Generation+1
	})
	if d := time.Since(killed); d < testConfig().LeaseTimeout {
		t.Errorf("rescheduled after %s, before the lease expired", d)
	}
}

func TestFailingProbeIsReplaced(t *testing.T) {
	f := newFakeCluster("n1")
	s, stop := start(t, f, filepath.Join(t.TempDir(), "db"), testConfig())
	defer stop()
	apply(t, s, orch.PodSpec{Name: "web", Image: "nginx", Probe: &orch.Probe{HTTP: &orch.HTTPProbe{Port: 80}}})
	eventually(t, "web running", func() bool { _, ok := running(s, "web"); return ok })

	f.set(func() {
		for _, c := range f.containers["n1"] {
			c.Healthy, c.ProbeFailures = false, 3
		}
	})
	eventually(t, "generation 2 running, generation 1 gone", func() bool {
		p, ok := running(s, "web")
		f.mu.Lock()
		_, oldGone := f.containers["n1"][runKey("web", 1)]
		f.mu.Unlock()
		return ok && p.Status.Generation == 2 && p.Status.Restarts == 1 && !oldGone
	})
}

func TestExitedContainerIsReplaced(t *testing.T) {
	f := newFakeCluster("n1")
	s, stop := start(t, f, filepath.Join(t.TempDir(), "db"), testConfig())
	defer stop()
	apply(t, s, orch.PodSpec{Name: "job", Image: "alpine"})
	eventually(t, "running", func() bool { _, ok := running(s, "job"); return ok })
	f.set(func() {
		for _, c := range f.containers["n1"] {
			c.State, c.ExitCode = orch.StateExited, 1
		}
	})
	eventually(t, "generation 2 running", func() bool { p, ok := running(s, "job"); return ok && p.Status.Generation == 2 })
}

// A node that is slow rather than dead gets its pod rescheduled; when it
// comes back, the copy it kept running must be stopped.
func TestStaleCopyStoppedWhenNodeReturns(t *testing.T) {
	f := newFakeCluster("n1", "n2")
	s, stop := start(t, f, filepath.Join(t.TempDir(), "db"), testConfig())
	defer stop()
	apply(t, s, orch.PodSpec{Name: "web", Image: "nginx"})
	var first *orch.Pod
	eventually(t, "web running", func() bool { var ok bool; first, ok = running(s, "web"); return ok })
	slow := first.Status.Node

	f.set(func() { f.paused[slow] = true })
	eventually(t, "duplicate window: copies on both nodes", func() bool { return len(f.where("web")) == 2 })
	f.set(func() { f.paused[slow] = false })
	eventually(t, "only the new copy remains", func() bool {
		w := f.where("web")
		return len(w) == 1 && w[0] != slow
	})
}

func TestDeleteStopsContainer(t *testing.T) {
	f := newFakeCluster("n1")
	s, stop := start(t, f, filepath.Join(t.TempDir(), "db"), testConfig())
	defer stop()
	apply(t, s, orch.PodSpec{Name: "web", Image: "nginx"})
	eventually(t, "running", func() bool { _, ok := running(s, "web"); return ok })
	if err := s.Delete("web"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "container stopped", func() bool { return len(f.where("web")) == 0 })
}

func TestPendingWithoutCapacity(t *testing.T) {
	f := newFakeCluster("n1")
	s, stop := start(t, f, filepath.Join(t.TempDir(), "db"), testConfig())
	defer stop()
	apply(t, s, orch.PodSpec{Name: "big", Image: "nginx", Resources: orch.Resources{Memory: "4g"}})
	time.Sleep(200 * time.Millisecond)
	if p, _ := s.Pod("big"); p.Status.Phase != orch.Pending || p.Status.Node != "" {
		t.Fatalf("got %+v, want Pending with no node", p.Status)
	}
	f.set(func() { f.memory = 8 << 30 })
	eventually(t, "scheduled once a node has room", func() bool { _, ok := running(s, "big"); return ok })
}

func TestRestartKeepsPlacements(t *testing.T) {
	f := newFakeCluster("n1", "n2")
	db := filepath.Join(t.TempDir(), "db")
	s, stop := start(t, f, db, testConfig())
	apply(t, s, orch.PodSpec{Name: "web", Image: "nginx"})
	var before *orch.Pod
	eventually(t, "running", func() bool { var ok bool; before, ok = running(s, "web"); return ok })
	stop()

	cfg := testConfig()
	cfg.StartupGrace = 500 * time.Millisecond
	s2, stop2 := start(t, f, db, cfg)
	defer stop2()
	time.Sleep(800 * time.Millisecond)
	p, _ := s2.Pod("web")
	if p.Status.Node != before.Status.Node || p.Status.Generation != before.Status.Generation {
		t.Fatalf("restart moved the pod: before %+v, after %+v", before.Status, p.Status)
	}
	if w := f.where("web"); len(w) != 1 {
		t.Fatalf("containers after restart: %v", w)
	}
}

func TestApplyValidates(t *testing.T) {
	f := newFakeCluster("n1")
	s, stop := start(t, f, filepath.Join(t.TempDir(), "db"), testConfig())
	defer stop()
	for _, bad := range []orch.PodSpec{
		{Name: "Bad_Name", Image: "nginx"},
		{Name: "x", Image: "Not Valid"},
		{Name: "x", Image: "nginx", Resources: orch.Resources{Memory: "lots"}},
		{Name: "x", Image: "nginx", Probe: &orch.Probe{}},
	} {
		if _, err := s.Apply(bad); err == nil {
			t.Errorf("Apply(%+v) succeeded, want error", bad)
		}
	}
}

func TestCrashLoopBacksOff(t *testing.T) {
	f := newFakeCluster("n1")
	f.crashing["bad"] = true
	s, stop := start(t, f, filepath.Join(t.TempDir(), "db"), testConfig())
	defer stop()
	apply(t, s, orch.PodSpec{Name: "bad", Image: "alpine"})
	// Without back-off this would reach dozens of generations in a second.
	// With 200ms doubling it gets to about 5: 0, 0, 0.2, 0.4, 0.8s gaps.
	time.Sleep(1500 * time.Millisecond)
	p, _ := s.Pod("bad")
	if p.Status.Generation > 6 || p.Status.Generation < 3 {
		t.Fatalf("generation %d after 1.5s, want 3-6 with back-off", p.Status.Generation)
	}
	if !strings.Contains(p.Status.Message, "back-off") && p.Status.Phase != orch.Starting {
		t.Errorf("status = %+v, want a back-off message", p.Status)
	}
	// Once the pod can run, it recovers and the failure count resets.
	f.set(func() { f.crashing["bad"] = false })
	eventually(t, "recovered and stable", func() bool {
		p, ok := running(s, "bad")
		return ok && p.Status.Failures == 0
	})
}
