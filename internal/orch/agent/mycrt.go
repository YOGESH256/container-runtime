package agent

import (
	"bytes"
	"context"
	"encoding/json"

	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/YOGESH256/container-runtime/internal/orch"
)

// Labels mycrt stores on pod containers.
const (
	labelPod   = "orch.pod"
	labelGen   = "orch.gen"
	labelProbe = "orch.probe"
)

// Mycrt is the Runtime that drives the mycrt command.
type Mycrt struct {
	Path string
}

// List returns the pod containers mycrt knows about, running or not.
func (m Mycrt) List(ctx context.Context) ([]Container, error) {
	out, err := m.run(ctx, "ps", "-a", "--json")
	if err != nil {
		return nil, err
	}
	var states []struct {
		ID       string            `json:"id"`
		Name     string            `json:"name"`
		Labels   map[string]string `json:"labels"`
		Status   string            `json:"status"`
		IP       string            `json:"ip"`
		ExitCode int               `json:"exitCode"`
		Error    string            `json:"error"`
	}
	if err := json.Unmarshal(out, &states); err != nil {
		return nil, fmt.Errorf("parse mycrt ps: %w", err)
	}
	var list []Container
	for _, s := range states {
		pod := s.Labels[labelPod]
		gen, err := strconv.ParseInt(s.Labels[labelGen], 10, 64)
		if pod == "" || err != nil {
			continue // not a pod container
		}
		c := Container{ID: s.ID, Name: s.Name, Pod: pod, Generation: gen, Status: s.Status,
			IP: s.IP, ExitCode: s.ExitCode, Error: s.Error}
		if p := s.Labels[labelProbe]; p != "" {
			c.Probe = &orch.Probe{}
			if json.Unmarshal([]byte(p), c.Probe) != nil {
				c.Probe = nil
			}
		}
		list = append(list, c)
	}
	return list, nil
}

// Run starts a pod generation as a detached container. The probe travels
// as a label, so it survives an agent restart along with the container.
func (m Mycrt) Run(ctx context.Context, req orch.RunRequest) error {
	s := req.Spec
	// --init: PID 1 in a long-running pod must reap orphans and relay
	// signals, and most server programs were not written to be PID 1.
	args := []string{"run", "-d", "--init",
		"--name", orch.ContainerName(req.Pod, req.Generation),
		"--hostname", req.Pod,
		"--label", labelPod + "=" + req.Pod,
		"--label", labelGen + "=" + strconv.FormatInt(req.Generation, 10),
	}
	if s.Probe != nil {
		b, _ := json.Marshal(s.Probe)
		args = append(args, "--label", labelProbe+"="+string(b))
	}
	if s.Resources.Memory != "" {
		args = append(args, "--memory", s.Resources.Memory)
	}
	if s.Resources.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(s.Resources.CPUs, 'f', -1, 64))
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+s.Env[k])
	}
	args = append(args, s.Image)
	args = append(args, s.Command...)
	_, err := m.run(ctx, args...)
	return err
}

// Remove stops a container gracefully, then deletes it.
func (m Mycrt) Remove(ctx context.Context, name string) error {
	if _, err := m.run(ctx, "stop", "-t", "10", name); err != nil && !isNoSuch(err) {
		return err
	}
	if _, err := m.run(ctx, "rm", "-f", name); err != nil && !isNoSuch(err) {
		return err
	}
	return nil
}

// Pull downloads an image.
func (m Mycrt) Pull(ctx context.Context, image string) error {
	_, err := m.run(ctx, "pull", image)
	return err
}

func (m Mycrt) run(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, m.Path, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return nil, fmt.Errorf("mycrt %s: %s", args[0], strings.TrimPrefix(msg, "mycrt: "))
	}
	return stdout.Bytes(), nil
}

func isNoSuch(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such container")
}
