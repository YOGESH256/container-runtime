// Package orch holds the objects shared by the control plane, the node
// agents and myctl.
//
// The orchestrator is one reconcile loop: observe what is actually
// running, compare it with what should be running, act on the difference,
// repeat. Users declare pods (desired state). Node agents report the
// containers they run (actual state) in heartbeats. The control plane
// closes the gap by telling agents to start and stop containers.
package orch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/YOGESH256/container-runtime/internal/image"
	"github.com/YOGESH256/container-runtime/internal/units"
)

// PodSpec is what a user asks for: one container, kept running somewhere.
type PodSpec struct {
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Command   []string          `json:"command,omitempty"` // replaces the image's Cmd
	Env       map[string]string `json:"env,omitempty"`
	Resources Resources         `json:"resources,omitzero"`
	Probe     *Probe            `json:"probe,omitempty"`
}

// Resources are the container's limits. The scheduler also treats them as
// what the pod needs from a node.
type Resources struct {
	Memory string  `json:"memory,omitempty"` // e.g. "128m"
	CPUs   float64 `json:"cpus,omitempty"`
}

// Probe checks that a running container is healthy.
type Probe struct {
	HTTP                *HTTPProbe `json:"http,omitempty"`
	TCP                 *TCPProbe  `json:"tcp,omitempty"`
	InitialDelaySeconds int        `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int        `json:"periodSeconds,omitempty"`
	TimeoutSeconds      int        `json:"timeoutSeconds,omitempty"`
	FailureThreshold    int        `json:"failureThreshold,omitempty"`
}

// HTTPProbe passes on any 2xx or 3xx response.
type HTTPProbe struct {
	Port int    `json:"port"`
	Path string `json:"path,omitempty"`
}

// TCPProbe passes if a connection opens.
type TCPProbe struct {
	Port int `json:"port"`
}

var podNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// DefaultMemory caps a pod that gives no memory limit, so one forgotten
// field cannot let a pod eat a whole node.
const DefaultMemory = "256m"

// Normalize fills in defaults and validates the spec.
func (p *PodSpec) Normalize() error {
	if !podNameRe.MatchString(p.Name) {
		return fmt.Errorf("pod name %q: use 1-40 lowercase letters, digits and '-'", p.Name)
	}
	if p.Resources.Memory == "" {
		p.Resources.Memory = DefaultMemory
	}
	if _, err := image.ParseReference(p.Image); err != nil {
		return fmt.Errorf("pod %s: %v", p.Name, err)
	}
	if _, err := p.MemoryBytes(); err != nil {
		return fmt.Errorf("pod %s: resources.memory: %v", p.Name, err)
	}
	if p.Resources.CPUs < 0 {
		return fmt.Errorf("pod %s: resources.cpus must not be negative", p.Name)
	}
	if pr := p.Probe; pr != nil {
		if (pr.HTTP == nil) == (pr.TCP == nil) {
			return fmt.Errorf("pod %s: a probe needs exactly one of http or tcp", p.Name)
		}
		if pr.HTTP != nil && pr.HTTP.Path == "" {
			pr.HTTP.Path = "/"
		}
		if pr.PeriodSeconds <= 0 {
			pr.PeriodSeconds = 3
		}
		if pr.TimeoutSeconds <= 0 {
			pr.TimeoutSeconds = 1
		}
		if pr.FailureThreshold <= 0 {
			pr.FailureThreshold = 3
		}
	}
	return nil
}

// MemoryBytes parses Resources.Memory; zero means no limit.
func (p *PodSpec) MemoryBytes() (int64, error) {
	if p.Resources.Memory == "" {
		return 0, nil
	}
	return units.ParseBytes(p.Resources.Memory)
}

// Hash identifies the spec's content. A new hash means the running
// container no longer matches what the user asked for.
func (p *PodSpec) Hash() string {
	b, _ := json.Marshal(p) // map keys are sorted, so this is stable
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// Phase summarizes where a pod is.
type Phase string

const (
	Pending  Phase = "Pending"  // no node can take it yet
	Starting Phase = "Starting" // placed; container starting or not yet healthy
	Running  Phase = "Running"  // container running and passing its probe
)

// PodStatus is the control plane's view of a pod.
type PodStatus struct {
	Phase       Phase     `json:"phase"`
	Node        string    `json:"node,omitempty"`
	Generation  int64     `json:"generation"` // bumped on every placement
	SpecHash    string    `json:"specHash,omitempty"`
	ContainerID string    `json:"containerId,omitempty"`
	IP          string    `json:"ip,omitempty"`
	Restarts    int       `json:"restarts"`
	Failures    int       `json:"failures,omitempty"` // consecutive quick failures, for back-off
	NextAttempt time.Time `json:"nextAttempt,omitzero"`
	Message     string    `json:"message,omitempty"`
	AssignedAt  time.Time `json:"assignedAt,omitzero"`
	RunningAt   time.Time `json:"runningAt,omitzero"`
}

// Pod is a spec with its status.
type Pod struct {
	Spec   PodSpec   `json:"spec"`
	Status PodStatus `json:"status"`
}

// Capacity is what a node offers.
type Capacity struct {
	MemoryBytes int64   `json:"memoryBytes"`
	CPUs        float64 `json:"cpus"`
}

// Container states reported by agents.
const (
	StateStarting = "starting" // requested, image pulling or container being created
	StateRunning  = "running"
	StateExited   = "exited"
	StateFailed   = "failed" // could not be started at all
)

// ContainerReport is one container as its node agent sees it.
type ContainerReport struct {
	ID            string `json:"id,omitempty"`
	Pod           string `json:"pod"`
	Generation    int64  `json:"generation"`
	State         string `json:"state"`
	IP            string `json:"ip,omitempty"`
	ExitCode      int    `json:"exitCode,omitempty"`
	Healthy       bool   `json:"healthy"`
	ProbeFailures int    `json:"probeFailures,omitempty"`
	Message       string `json:"message,omitempty"`
}

// Heartbeat is what an agent sends every few seconds.
type Heartbeat struct {
	Node       string            `json:"node"`
	Address    string            `json:"address"` // agent API base URL
	Capacity   Capacity          `json:"capacity"`
	Containers []ContainerReport `json:"containers"`
}

// HeartbeatResponse tells the agent which images the cluster uses, so it
// can pull them ahead of time and take over pods quickly.
type HeartbeatResponse struct {
	Images []string `json:"images"`
}

// RunRequest asks an agent to start one generation of a pod.
type RunRequest struct {
	Pod        string  `json:"pod"`
	Generation int64   `json:"generation"`
	Spec       PodSpec `json:"spec"`
}

// Node is the control plane's view of a node.
type Node struct {
	Name          string            `json:"name"`
	Address       string            `json:"address"`
	Ready         bool              `json:"ready"`
	LastHeartbeat time.Time         `json:"lastHeartbeat"`
	Capacity      Capacity          `json:"capacity"`
	Containers    []ContainerReport `json:"containers"`
}

// Event is a line in the control plane's activity log.
type Event struct {
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"time"`
	Pod     string    `json:"pod,omitempty"`
	Node    string    `json:"node,omitempty"`
	Message string    `json:"message"`
}

// ContainerName is the mycrt container name for a pod generation.
func ContainerName(pod string, gen int64) string {
	return pod + "-g" + strconv.FormatInt(gen, 10)
}
