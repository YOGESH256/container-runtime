package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/YOGESH256/container-runtime/internal/orch"
)

// fakeRuntime keeps containers in memory.
type fakeRuntime struct {
	mu      sync.Mutex
	list    []Container
	failRun error
	ip      string
	port    int
}

func (f *fakeRuntime) List(context.Context) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Container(nil), f.list...), nil
}

func (f *fakeRuntime) Run(_ context.Context, req orch.RunRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRun != nil {
		return f.failRun
	}
	f.list = append(f.list, Container{ID: "c" + strconv.Itoa(len(f.list)), Name: orch.ContainerName(req.Pod, req.Generation),
		Pod: req.Pod, Generation: req.Generation, Status: "running", IP: f.ip, Probe: req.Spec.Probe})
	return nil
}

func (f *fakeRuntime) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.list {
		if c.Name == name {
			f.list = append(f.list[:i], f.list[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeRuntime) Pull(context.Context, string) error { return nil }

func waitReport(t *testing.T, a *Agent, cond func([]orch.ContainerReport) bool) []orch.ContainerReport {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		r, err := a.Reports(context.Background())
		if err == nil && cond(r) {
			return r
		}
	}
	r, _ := a.Reports(context.Background())
	t.Fatalf("condition not reached; last reports: %+v", r)
	return nil
}

func TestStartIsIdempotentAndReported(t *testing.T) {
	rt := &fakeRuntime{ip: "10.88.0.9"}
	a := New(Config{Node: "n1"}, rt)
	req := orch.RunRequest{Pod: "web", Generation: 1, Spec: orch.PodSpec{Name: "web", Image: "nginx"}}
	a.Start(req)
	a.Start(req)
	waitReport(t, a, func(r []orch.ContainerReport) bool {
		return len(r) == 1 && r[0].State == orch.StateRunning && r[0].Healthy
	})
	a.Start(req) // a retry after it is running must not start a second copy
	time.Sleep(100 * time.Millisecond)
	if l, _ := rt.List(context.Background()); len(l) != 1 {
		t.Fatalf("containers = %d, want 1", len(l))
	}
	if err := a.Stop(context.Background(), "web", 1); err != nil {
		t.Fatal(err)
	}
	waitReport(t, a, func(r []orch.ContainerReport) bool { return len(r) == 0 })
}

func TestFailedStartIsReported(t *testing.T) {
	rt := &fakeRuntime{failRun: errors.New("mycrt run: image not found")}
	a := New(Config{Node: "n1"}, rt)
	a.Start(orch.RunRequest{Pod: "web", Generation: 3, Spec: orch.PodSpec{Name: "web", Image: "nope"}})
	r := waitReport(t, a, func(r []orch.ContainerReport) bool { return len(r) == 1 && r[0].State == orch.StateFailed })
	if r[0].Generation != 3 || r[0].Message == "" {
		t.Fatalf("report = %+v", r[0])
	}
}

func TestProbeCountsFailures(t *testing.T) {
	var healthy sync.Mutex
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthy.Lock()
		defer healthy.Unlock()
		if !up {
			http.Error(w, "down", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	rt := &fakeRuntime{ip: host}
	a := New(Config{Node: "n1"}, rt)
	probe := &orch.Probe{HTTP: &orch.HTTPProbe{Port: port, Path: "/"}, PeriodSeconds: 1, TimeoutSeconds: 1, FailureThreshold: 2}
	a.Start(orch.RunRequest{Pod: "web", Generation: 1, Spec: orch.PodSpec{Name: "web", Image: "nginx", Probe: probe}})
	waitReport(t, a, func(r []orch.ContainerReport) bool { return len(r) == 1 && r[0].Healthy })

	healthy.Lock()
	up = false
	healthy.Unlock()
	waitReport(t, a, func(r []orch.ContainerReport) bool { return len(r) == 1 && !r[0].Healthy && r[0].ProbeFailures >= 2 })

	healthy.Lock()
	up = true
	healthy.Unlock()
	waitReport(t, a, func(r []orch.ContainerReport) bool { return len(r) == 1 && r[0].Healthy && r[0].ProbeFailures == 0 })
}

func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ok := check(context.Background(), orch.Probe{TCP: &orch.TCPProbe{Port: port}}, "127.0.0.1", time.Second)
	ln.Close()
	bad := check(context.Background(), orch.Probe{TCP: &orch.TCPProbe{Port: port}}, "127.0.0.1", time.Second)
	if !ok || bad {
		t.Fatalf("open port: %v, closed port: %v", ok, bad)
	}
}
