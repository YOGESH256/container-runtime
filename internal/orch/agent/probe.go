package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/YOGESH256/container-runtime/internal/orch"
)

// prober checks one container on a timer. The agent runs probes, not the
// control plane, because container IPs are private to their node's bridge.
type prober struct {
	mu       sync.Mutex
	healthy  bool
	failures int // consecutive
	cancel   context.CancelFunc
}

func startProber(spec orch.Probe, ip string) *prober {
	ctx, cancel := context.WithCancel(context.Background())
	p := &prober{cancel: cancel}
	go p.run(ctx, spec, ip)
	return p
}

func (p *prober) run(ctx context.Context, spec orch.Probe, ip string) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(spec.InitialDelaySeconds) * time.Second):
	}
	timeout := time.Duration(spec.TimeoutSeconds) * time.Second
	for {
		ok := check(ctx, spec, ip, timeout)
		p.mu.Lock()
		if ok {
			p.healthy, p.failures = true, 0
		} else {
			p.healthy, p.failures = false, p.failures+1
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(spec.PeriodSeconds) * time.Second):
		}
	}
}

func (p *prober) status() (healthy bool, failures int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.healthy, p.failures
}

func (p *prober) stop() { p.cancel() }

// check runs one probe. HTTP passes on 2xx and 3xx; TCP on a connection.
func check(ctx context.Context, spec orch.Probe, ip string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if spec.TCP != nil {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(spec.TCP.Port)))
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	url := fmt.Sprintf("http://%s%s", net.JoinHostPort(ip, strconv.Itoa(spec.HTTP.Port)), spec.HTTP.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	// No redirects: a 3xx already counts as healthy.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}
