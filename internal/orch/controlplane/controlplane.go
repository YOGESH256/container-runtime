// Package controlplane keeps pods running across a cluster of nodes.
//
// State lives in two places. Desired state is the pods users applied,
// stored in bbolt together with where each one was placed. Actual state is
// whatever the node agents last reported in their heartbeats, kept in
// memory. Once a second the reconcile loop compares the two and acts:
//
//   - a pod with no node gets scheduled onto the least loaded ready node;
//   - a pod on a node that stopped sending heartbeats is placed elsewhere;
//   - a container that exited, failed to start, or failed its health probe
//     too many times in a row is replaced with a new generation;
//   - a container that is no longer wanted (deleted pod, old generation,
//     or a copy on a node that was declared dead and came back) is stopped.
//
// Every placement bumps the pod's generation, and agents label containers
// with it. That is what lets the control plane tell the current container
// apart from a stale copy of the same pod.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"container-runtime/internal/orch"

	bolt "go.etcd.io/bbolt"
)

// Config holds the timing knobs.
type Config struct {
	// LeaseTimeout is how long a node may go without a heartbeat before it
	// is declared dead and its pods are rescheduled. Shorter means faster
	// failover but a higher chance of rescheduling off a node that is only
	// slow, which briefly runs two copies of its pods.
	LeaseTimeout time.Duration
	// ReconcileEvery is the loop period.
	ReconcileEvery time.Duration
	// StartupGrace delays rescheduling after the control plane starts, so
	// nodes have time to send their first heartbeat.
	StartupGrace time.Duration
	// RunResend is how often an unanswered run request is repeated.
	RunResend time.Duration
	// StartTimeout gives up on a container that never starts.
	StartTimeout time.Duration
	// StopResend is how often a stop request for a stale container is repeated.
	StopResend time.Duration
	// A pod whose containers keep failing soon after starting is retried
	// after BackoffBase, doubling each time up to BackoffMax, instead of in
	// a tight loop. A container healthy for StableAfter resets the count.
	BackoffBase, BackoffMax, StableAfter time.Duration
}

// DefaultConfig suits the three-VM demo cluster.
func DefaultConfig() Config {
	return Config{
		LeaseTimeout:   10 * time.Second,
		ReconcileEvery: time.Second,
		StartupGrace:   12 * time.Second,
		RunResend:      10 * time.Second,
		StartTimeout:   3 * time.Minute,
		StopResend:     5 * time.Second,
		BackoffBase:    2 * time.Second,
		BackoffMax:     time.Minute,
		StableAfter:    30 * time.Second,
	}
}

// AgentClient sends commands to node agents.
type AgentClient interface {
	Run(ctx context.Context, addr string, req orch.RunRequest) error
	Stop(ctx context.Context, addr, pod string, gen int64) error
}

var bucketPods = []byte("pods")

// Server is the control plane.
type Server struct {
	cfg    Config
	db     *bolt.DB
	agents AgentClient

	mu       sync.Mutex
	started  time.Time
	nodes    map[string]*orch.Node
	lastRun  map[string]time.Time // pod/generation -> last run request
	lastStop map[string]time.Time // node/pod/generation -> last stop request
	events   []orch.Event
	eventSeq int64
	kick     chan struct{}
}

// Open loads (or creates) the pod database at path.
func Open(path string, cfg Config, agents AgentClient) (*Server, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketPods)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &Server{
		cfg: cfg, db: db, agents: agents, started: time.Now(),
		nodes: map[string]*orch.Node{}, lastRun: map[string]time.Time{}, lastStop: map[string]time.Time{},
		kick: make(chan struct{}, 1),
	}, nil
}

// Close releases the database.
func (s *Server) Close() error { return s.db.Close() }

// Run drives the reconcile loop until ctx is done.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.ReconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
		s.Reconcile()
	}
}

func (s *Server) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Apply creates or updates a pod. Updating the spec of a placed pod makes
// the next reconcile replace its container.
func (s *Server) Apply(spec orch.PodSpec) (*orch.Pod, error) {
	if err := spec.Normalize(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pod, err := s.getPod(spec.Name)
	switch {
	case err == nil && pod.Spec.Hash() == spec.Hash():
		return pod, nil // unchanged
	case err == nil:
		pod.Spec = spec
		s.eventf(spec.Name, "", "spec updated")
	case errors.Is(err, errNoPod):
		pod = &orch.Pod{Spec: spec, Status: orch.PodStatus{Phase: orch.Pending, Message: "waiting to be scheduled"}}
		s.eventf(spec.Name, "", "created")
	default:
		return nil, err
	}
	if err := s.putPod(pod); err != nil {
		return nil, err
	}
	s.poke()
	return pod, nil
}

// Delete removes a pod. Its container is stopped by the next reconcile.
func (s *Server) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.getPod(name); err != nil {
		return err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketPods).Delete([]byte(name)) }); err != nil {
		return err
	}
	s.eventf(name, "", "deleted")
	s.poke()
	return nil
}

// Pods lists every pod, sorted by name.
func (s *Server) Pods() ([]*orch.Pod, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listPods()
}

// Pod returns one pod.
func (s *Server) Pod(name string) (*orch.Pod, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getPod(name)
}

// Nodes lists the nodes seen so far, with their readiness.
func (s *Server) Nodes() []orch.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshReadiness(time.Now())
	out := make([]orch.Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Events returns log entries newer than seq.
func (s *Server) Events(since int64) []orch.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []orch.Event
	for _, e := range s.events {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	return out
}

// Heartbeat records a node's report and answers with the images the
// cluster uses.
func (s *Server) Heartbeat(hb orch.Heartbeat) orch.HeartbeatResponse {
	s.mu.Lock()
	n, known := s.nodes[hb.Node]
	if !known {
		n = &orch.Node{Name: hb.Node}
		s.nodes[hb.Node] = n
		s.eventf("", hb.Node, "node joined from %s", hb.Address)
	} else if !n.Ready {
		s.eventf("", hb.Node, "node is sending heartbeats again")
	}
	n.Address, n.Capacity, n.Containers = hb.Address, hb.Capacity, hb.Containers
	n.LastHeartbeat, n.Ready = time.Now(), true

	seen := map[string]bool{}
	var images []string
	if pods, err := s.listPods(); err == nil {
		for _, p := range pods {
			if !seen[p.Spec.Image] {
				seen[p.Spec.Image] = true
				images = append(images, p.Spec.Image)
			}
		}
	}
	s.mu.Unlock()
	s.poke()
	return orch.HeartbeatResponse{Images: images}
}

// Reconcile runs one pass of the control loop.
func (s *Server) Reconcile() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.refreshReadiness(now)
	pods, err := s.listPods()
	if err != nil {
		s.eventf("", "", "reconcile: %v", err)
		return
	}
	byName := map[string]*orch.Pod{}
	for _, p := range pods {
		byName[p.Spec.Name] = p
		s.reconcilePod(p, now)
	}
	s.stopUnwanted(byName, now)
}

func (s *Server) refreshReadiness(now time.Time) {
	for _, n := range s.nodes {
		if n.Ready && now.Sub(n.LastHeartbeat) > s.cfg.LeaseTimeout {
			n.Ready = false
			s.eventf("", n.Name, "node not ready: no heartbeat for %s", now.Sub(n.LastHeartbeat).Round(time.Second))
		}
	}
}

func (s *Server) reconcilePod(pod *orch.Pod, now time.Time) {
	st := &pod.Status
	if st.Node == "" {
		if now.Before(st.NextAttempt) {
			return // backing off after repeated failures
		}
		s.place(pod, "scheduled", now)
		return
	}
	node := s.nodes[st.Node]
	if node == nil || !node.Ready {
		// Right after a control-plane restart no node has reported yet.
		if now.Sub(s.started) < s.cfg.StartupGrace {
			return
		}
		// Not the pod's fault, so no back-off.
		s.replace(pod, fmt.Sprintf("node %s is not ready", st.Node), false, now)
		return
	}
	if st.SpecHash != pod.Spec.Hash() {
		st.Failures = 0 // a new spec starts with a clean record
		s.replace(pod, "spec changed", false, now)
		return
	}

	rep := findReport(node, pod.Spec.Name, st.Generation)
	switch {
	case rep == nil || rep.State == orch.StateStarting:
		s.setPhase(pod, orch.Starting, "starting on "+node.Name)
		key := runKey(pod.Spec.Name, st.Generation)
		if rep == nil && now.Sub(s.lastRun[key]) > s.cfg.RunResend {
			s.sendRun(node, pod)
		}
		if now.Sub(st.AssignedAt) > s.cfg.StartTimeout {
			s.replace(pod, fmt.Sprintf("did not start within %s", s.cfg.StartTimeout), true, now)
		}
	case rep.State == orch.StateFailed:
		s.replace(pod, "failed to start: "+rep.Message, true, now)
	case rep.State == orch.StateExited:
		s.replace(pod, fmt.Sprintf("container exited with code %d", rep.ExitCode), true, now)
	case rep.State == orch.StateRunning:
		if pr := pod.Spec.Probe; pr != nil && rep.ProbeFailures >= pr.FailureThreshold {
			s.replace(pod, fmt.Sprintf("health probe failed %d times in a row", rep.ProbeFailures), true, now)
			return
		}
		changed := st.ContainerID != rep.ID || st.IP != rep.IP
		st.ContainerID, st.IP = rep.ID, rep.IP
		if st.Failures > 0 && !st.RunningAt.IsZero() && now.Sub(st.RunningAt) > s.cfg.StableAfter {
			st.Failures, changed = 0, true
		}
		if pod.Spec.Probe == nil || rep.Healthy {
			if st.Phase != orch.Running {
				st.RunningAt = now
				s.eventf(pod.Spec.Name, node.Name, "running (generation %d, %s, up %s after placement)",
					st.Generation, rep.IP, now.Sub(st.AssignedAt).Round(100*time.Millisecond))
			}
			s.setPhase(pod, orch.Running, "")
		} else {
			s.setPhase(pod, orch.Starting, "waiting for the health probe to pass")
		}
		if changed {
			s.putPod(pod)
		}
	}
}

// replace stops a pod's current container (if its node can still be
// reached) and places a new generation. podFault says whether the pod
// itself failed, as opposed to its node. Repeated quick failures back off
// exponentially, like Kubernetes' CrashLoopBackOff, so a pod that can
// never start does not hammer a node with a restart every second.
func (s *Server) replace(pod *orch.Pod, reason string, podFault bool, now time.Time) {
	st := &pod.Status
	if n := s.nodes[st.Node]; n != nil && n.Ready {
		s.sendStop(n, pod.Spec.Name, st.Generation, now)
	}
	st.Restarts++
	s.eventf(pod.Spec.Name, st.Node, "replacing generation %d: %s", st.Generation, reason)
	if podFault {
		if st.RunningAt.IsZero() || now.Sub(st.RunningAt) < s.cfg.StableAfter {
			st.Failures++
		} else {
			st.Failures = 1
		}
		if st.Failures >= 2 {
			delay := s.cfg.BackoffBase << (st.Failures - 2)
			if delay > s.cfg.BackoffMax || delay <= 0 {
				delay = s.cfg.BackoffMax
			}
			st.NextAttempt = now.Add(delay)
			st.Phase, st.Node, st.ContainerID, st.IP = orch.Pending, "", "", ""
			st.Message = fmt.Sprintf("back-off %s after %d failures in a row: %s", delay, st.Failures, reason)
			s.putPod(pod)
			s.eventf(pod.Spec.Name, "", "%s", st.Message)
			return
		}
	}
	s.place(pod, reason, now)
}

// place schedules a new generation of the pod onto a node.
func (s *Server) place(pod *orch.Pod, reason string, now time.Time) {
	st := &pod.Status
	node := s.pick(pod)
	if node == nil {
		msg := "no ready node has room for it"
		if st.Phase != orch.Pending || st.Message != msg || st.Node != "" {
			st.Phase, st.Node, st.Message, st.ContainerID, st.IP = orch.Pending, "", msg, "", ""
			s.putPod(pod)
			s.eventf(pod.Spec.Name, "", "pending: %s", msg)
		}
		return
	}
	st.Generation++
	st.Node, st.Phase, st.Message = node.Name, orch.Starting, reason
	st.AssignedAt, st.RunningAt, st.SpecHash = now, time.Time{}, pod.Spec.Hash()
	st.ContainerID, st.IP = "", ""
	s.putPod(pod)
	s.eventf(pod.Spec.Name, node.Name, "placed generation %d on %s (%s)", st.Generation, node.Name, reason)
	s.sendRun(node, pod)
}

// pick chooses the ready node with the fewest pods among those with room
// for this one. Ties go to the node with more free memory, then by name.
func (s *Server) pick(pod *orch.Pod) *orch.Node {
	need, _ := pod.Spec.MemoryBytes()
	type load struct {
		pods int
		mem  int64
		cpus float64
	}
	loads := map[string]*load{}
	for name := range s.nodes {
		loads[name] = &load{}
	}
	if pods, err := s.listPods(); err == nil {
		for _, p := range pods {
			if p.Spec.Name == pod.Spec.Name || p.Status.Node == "" {
				continue
			}
			if l := loads[p.Status.Node]; l != nil {
				m, _ := p.Spec.MemoryBytes()
				l.pods, l.mem, l.cpus = l.pods+1, l.mem+m, l.cpus+p.Spec.Resources.CPUs
			}
		}
	}
	var best *orch.Node
	var bestFree int64
	for _, n := range s.nodes {
		if !n.Ready {
			continue
		}
		l := loads[n.Name]
		free := n.Capacity.MemoryBytes - l.mem
		if n.Capacity.MemoryBytes > 0 && need > free {
			continue
		}
		if n.Capacity.CPUs > 0 && l.cpus+pod.Spec.Resources.CPUs > n.Capacity.CPUs {
			continue
		}
		if best == nil {
			best, bestFree = n, free
			continue
		}
		bl := loads[best.Name]
		if l.pods < bl.pods || (l.pods == bl.pods && (free > bestFree || (free == bestFree && n.Name < best.Name))) {
			best, bestFree = n, free
		}
	}
	return best
}

// stopUnwanted stops containers the cluster no longer wants: those of
// deleted pods, older generations, and copies left on a node that was
// declared dead and has come back.
func (s *Server) stopUnwanted(pods map[string]*orch.Pod, now time.Time) {
	for _, n := range s.nodes {
		if !n.Ready {
			continue
		}
		for _, rep := range n.Containers {
			p := pods[rep.Pod]
			if p != nil && p.Status.Node == n.Name && p.Status.Generation == rep.Generation {
				continue
			}
			s.sendStop(n, rep.Pod, rep.Generation, now)
		}
	}
}

func (s *Server) sendRun(n *orch.Node, pod *orch.Pod) {
	req := orch.RunRequest{Pod: pod.Spec.Name, Generation: pod.Status.Generation, Spec: pod.Spec}
	s.lastRun[runKey(req.Pod, req.Generation)] = time.Now()
	addr, node := n.Address, n.Name
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.agents.Run(ctx, addr, req); err != nil {
			s.mu.Lock()
			s.eventf(req.Pod, node, "run request for generation %d failed: %v", req.Generation, err)
			s.mu.Unlock()
		}
	}()
}

func (s *Server) sendStop(n *orch.Node, pod string, gen int64, now time.Time) {
	key := n.Name + "/" + runKey(pod, gen)
	if now.Sub(s.lastStop[key]) < s.cfg.StopResend {
		return
	}
	if s.lastStop[key].IsZero() {
		s.eventf(pod, n.Name, "stopping generation %d on %s", gen, n.Name)
	}
	s.lastStop[key] = now
	addr := n.Address
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s.agents.Stop(ctx, addr, pod, gen)
	}()
}

func (s *Server) setPhase(pod *orch.Pod, phase orch.Phase, msg string) {
	if pod.Status.Phase == phase && pod.Status.Message == msg {
		return
	}
	pod.Status.Phase, pod.Status.Message = phase, msg
	s.putPod(pod)
}

func (s *Server) eventf(pod, node, format string, args ...any) {
	s.eventSeq++
	s.events = append(s.events, orch.Event{Seq: s.eventSeq, Time: time.Now(), Pod: pod, Node: node,
		Message: fmt.Sprintf(format, args...)})
	if len(s.events) > 1000 {
		s.events = s.events[len(s.events)-1000:]
	}
}

func findReport(n *orch.Node, pod string, gen int64) *orch.ContainerReport {
	for i := range n.Containers {
		if c := &n.Containers[i]; c.Pod == pod && c.Generation == gen {
			return c
		}
	}
	return nil
}

func runKey(pod string, gen int64) string { return fmt.Sprintf("%s/%d", pod, gen) }

// Storage. Callers hold s.mu.

var errNoPod = errors.New("no such pod")

func (s *Server) getPod(name string) (*orch.Pod, error) {
	var pod *orch.Pod
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPods).Get([]byte(name))
		if b == nil {
			return fmt.Errorf("%w: %s", errNoPod, name)
		}
		pod = &orch.Pod{}
		return json.Unmarshal(b, pod)
	})
	return pod, err
}

func (s *Server) listPods() ([]*orch.Pod, error) {
	var out []*orch.Pod
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPods).ForEach(func(_, v []byte) error {
			p := &orch.Pod{}
			if err := json.Unmarshal(v, p); err != nil {
				return err
			}
			out = append(out, p)
			return nil
		})
	})
	return out, err // bbolt iterates in key order, so already sorted by name
}

func (s *Server) putPod(p *orch.Pod) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketPods).Put([]byte(p.Spec.Name), b) })
}

// IsNotFound reports whether err means the pod does not exist.
func IsNotFound(err error) bool { return errors.Is(err, errNoPod) }
