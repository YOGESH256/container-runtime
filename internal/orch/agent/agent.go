// Package agent is the node agent. It runs pod containers on this node
// through mycrt, probes their health, and reports everything to the
// control plane in a heartbeat every few seconds.
//
// The agent keeps almost no state of its own. mycrt records each container
// with labels naming its pod, generation and probe, so an agent that
// restarts rediscovers its containers from `mycrt ps`. Containers keep
// running while the agent is down, because each has its own mycrt
// supervisor process.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"container-runtime/internal/orch"
)

// Config configures an agent.
type Config struct {
	Node           string        // this node's name
	ControlPlane   string        // control plane base URL
	Advertise      string        // base URL the control plane uses to reach this agent
	HeartbeatEvery time.Duration // default 2s
}

// Container is a pod container as the runtime reports it.
type Container struct {
	ID, Name   string
	Pod        string
	Generation int64
	Status     string // mycrt's: created, running or exited
	IP         string
	ExitCode   int
	Error      string
	Probe      *orch.Probe
}

// Runtime manages containers on this node. The real one runs mycrt.
type Runtime interface {
	List(ctx context.Context) ([]Container, error)
	Run(ctx context.Context, req orch.RunRequest) error
	Remove(ctx context.Context, name string) error
	Pull(ctx context.Context, image string) error
}

// Agent is one node's agent.
type Agent struct {
	cfg  Config
	rt   Runtime
	http *http.Client

	mu       sync.Mutex
	starting map[string]*startOp // pod/generation -> run in progress or failed
	probers  map[string]*prober  // container ID -> prober
	wanted   []string            // images the cluster uses
	pulled   map[string]bool
}

type startOp struct {
	failed bool
	msg    string
}

// New returns an agent.
func New(cfg Config, rt Runtime) *Agent {
	if cfg.HeartbeatEvery == 0 {
		cfg.HeartbeatEvery = 2 * time.Second
	}
	return &Agent{cfg: cfg, rt: rt, http: &http.Client{Timeout: 5 * time.Second},
		starting: map[string]*startOp{}, probers: map[string]*prober{}, pulled: map[string]bool{}}
}

func key(pod string, gen int64) string { return pod + "/" + strconv.FormatInt(gen, 10) }

// Start begins starting a pod generation in the background. Repeated
// requests for the same generation are ignored, so the control plane may
// safely retry.
func (a *Agent) Start(req orch.RunRequest) {
	k := key(req.Pod, req.Generation)
	a.mu.Lock()
	if _, busy := a.starting[k]; busy {
		a.mu.Unlock()
		return
	}
	a.starting[k] = &startOp{}
	a.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		// A retried request or a restarted agent may find it already there.
		if list, err := a.rt.List(ctx); err == nil {
			for _, c := range list {
				if c.Pod == req.Pod && c.Generation == req.Generation {
					a.finishStart(k, nil)
					return
				}
			}
		}
		log.Printf("starting %s generation %d (%s)", req.Pod, req.Generation, req.Spec.Image)
		err := a.rt.Run(ctx, req)
		if err != nil {
			log.Printf("start %s generation %d: %v", req.Pod, req.Generation, err)
		}
		a.finishStart(k, err)
	}()
}

func (a *Agent) finishStart(k string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	op, ok := a.starting[k]
	if !ok {
		return // stopped meanwhile
	}
	if err != nil {
		op.failed, op.msg = true, err.Error()
		return
	}
	delete(a.starting, k)
}

// Stop stops and removes a pod generation. Stopping something that does
// not exist succeeds.
func (a *Agent) Stop(ctx context.Context, pod string, gen int64) error {
	a.mu.Lock()
	delete(a.starting, key(pod, gen))
	a.mu.Unlock()
	log.Printf("stopping %s generation %d", pod, gen)
	return a.rt.Remove(ctx, orch.ContainerName(pod, gen))
}

// Reports lists this node's pod containers for a heartbeat.
func (a *Agent) Reports(ctx context.Context) ([]orch.ContainerReport, error) {
	list, err := a.rt.List(ctx)
	if err != nil {
		return nil, err
	}
	a.syncProbers(list)

	a.mu.Lock()
	defer a.mu.Unlock()
	reports := []orch.ContainerReport{}
	present := map[string]bool{}
	for _, c := range list {
		present[key(c.Pod, c.Generation)] = true
		r := orch.ContainerReport{ID: c.ID, Pod: c.Pod, Generation: c.Generation, IP: c.IP}
		switch c.Status {
		case "running":
			r.State = orch.StateRunning
			if c.Probe == nil {
				r.Healthy = true
			} else if p := a.probers[c.ID]; p != nil {
				r.Healthy, r.ProbeFailures = p.status()
			}
		case "exited":
			r.State, r.ExitCode, r.Message = orch.StateExited, c.ExitCode, c.Error
		default:
			r.State = orch.StateStarting
		}
		reports = append(reports, r)
	}
	for k, op := range a.starting {
		if present[k] {
			continue
		}
		pod, genStr, _ := strings.Cut(k, "/")
		gen, _ := strconv.ParseInt(genStr, 10, 64)
		r := orch.ContainerReport{Pod: pod, Generation: gen, State: orch.StateStarting}
		if op.failed {
			r.State, r.Message = orch.StateFailed, op.msg
		}
		reports = append(reports, r)
	}
	return reports, nil
}

// syncProbers starts a prober for each running container with a probe
// and stops probers whose container is gone.
func (a *Agent) syncProbers(list []Container) {
	a.mu.Lock()
	defer a.mu.Unlock()
	live := map[string]bool{}
	for _, c := range list {
		if c.Status != "running" || c.Probe == nil || c.IP == "" {
			continue
		}
		live[c.ID] = true
		if a.probers[c.ID] == nil {
			a.probers[c.ID] = startProber(*c.Probe, c.IP)
		}
	}
	for id, p := range a.probers {
		if !live[id] {
			p.stop()
			delete(a.probers, id)
		}
	}
}

// RunHeartbeats reports to the control plane until ctx is done.
func (a *Agent) RunHeartbeats(ctx context.Context) {
	for {
		a.heartbeat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(a.cfg.HeartbeatEvery):
		}
	}
}

func (a *Agent) heartbeat(ctx context.Context) {
	reports, err := a.Reports(ctx)
	if err != nil {
		// Reporting nothing would look like every container vanished.
		log.Printf("skipping heartbeat: cannot list containers: %v", err)
		return
	}
	hb := orch.Heartbeat{Node: a.cfg.Node, Address: a.cfg.Advertise, Capacity: capacity(), Containers: reports}
	body, _ := json.Marshal(hb)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.ControlPlane+"/v1/heartbeat", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		log.Printf("heartbeat: %v", err)
		return
	}
	defer resp.Body.Close()
	var hr orch.HeartbeatResponse
	if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&hr) == nil {
		a.mu.Lock()
		a.wanted = hr.Images
		a.mu.Unlock()
	}
}

// RunPrepull pulls every image the cluster uses, one at a time, so this
// node can take over any pod without waiting for a download.
func (a *Agent) RunPrepull(ctx context.Context) {
	for {
		a.mu.Lock()
		var next string
		for _, img := range a.wanted {
			if !a.pulled[img] {
				next = img
				break
			}
		}
		a.mu.Unlock()
		if next != "" {
			if err := a.rt.Pull(ctx, next); err != nil {
				log.Printf("pre-pull %s: %v", next, err)
			} else {
				log.Printf("pre-pulled %s", next)
				a.mu.Lock()
				a.pulled[next] = true
				a.mu.Unlock()
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// Handler serves the agent API:
//
//	POST   /v1/containers              start a pod generation (RunRequest)
//	DELETE /v1/containers/{pod}/{gen}  stop and remove it
//	GET    /v1/containers              status of this node's pod containers
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/containers", func(w http.ResponseWriter, r *http.Request) {
		var req orch.RunRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := req.Spec.Normalize(); err != nil || req.Generation <= 0 || req.Pod != req.Spec.Name {
			http.Error(w, fmt.Sprintf("bad run request: %v", err), http.StatusBadRequest)
			return
		}
		a.Start(req)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("DELETE /v1/containers/{pod}/{gen}", func(w http.ResponseWriter, r *http.Request) {
		gen, err := strconv.ParseInt(r.PathValue("gen"), 10, 64)
		if err != nil {
			http.Error(w, "bad generation", http.StatusBadRequest)
			return
		}
		if err := a.Stop(r.Context(), r.PathValue("pod"), gen); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/containers", func(w http.ResponseWriter, r *http.Request) {
		reports, err := a.Reports(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reports)
	})
	return mux
}

// capacity reports this machine's memory and CPUs.
func capacity() orch.Capacity {
	c := orch.Capacity{CPUs: float64(runtime.NumCPU())}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
				kb, _ := strconv.ParseInt(f[1], 10, 64)
				c.MemoryBytes = kb << 10
			}
		}
	}
	return c
}

// AdvertiseAddress guesses the URL the control plane can reach this agent
// at: the local address the kernel picks for traffic to the control plane.
func AdvertiseAddress(controlPlane, port string) (string, error) {
	u, err := url.Parse(controlPlane)
	if err != nil {
		return "", err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "80")
	}
	conn, err := net.Dial("udp", host)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP
	return "http://" + net.JoinHostPort(ip.String(), port), nil
}
