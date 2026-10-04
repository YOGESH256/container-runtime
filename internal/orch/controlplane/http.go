package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/YOGESH256/container-runtime/internal/orch"
)

// Handler serves the control plane's HTTP API:
//
//	PUT    /v1/pods/{name}   apply a pod spec
//	GET    /v1/pods          list pods
//	GET    /v1/pods/{name}   one pod
//	DELETE /v1/pods/{name}   delete a pod
//	GET    /v1/nodes         list nodes
//	GET    /v1/events?since= activity log
//	POST   /v1/heartbeat     node agents report in here
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/pods/{name}", func(w http.ResponseWriter, r *http.Request) {
		var spec orch.PodSpec
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&spec); err != nil {
			httpError(w, http.StatusBadRequest, "bad pod spec: %v", err)
			return
		}
		if spec.Name == "" {
			spec.Name = r.PathValue("name")
		}
		if spec.Name != r.PathValue("name") {
			httpError(w, http.StatusBadRequest, "name in body %q does not match URL", spec.Name)
			return
		}
		pod, err := s.Apply(spec)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%v", err)
			return
		}
		writeJSON(w, pod)
	})
	mux.HandleFunc("GET /v1/pods", func(w http.ResponseWriter, r *http.Request) {
		pods, err := s.Pods()
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		if pods == nil {
			pods = []*orch.Pod{}
		}
		writeJSON(w, pods)
	})
	mux.HandleFunc("GET /v1/pods/{name}", func(w http.ResponseWriter, r *http.Request) {
		pod, err := s.Pod(r.PathValue("name"))
		if IsNotFound(err) {
			httpError(w, http.StatusNotFound, "%v", err)
			return
		}
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		writeJSON(w, pod)
	})
	mux.HandleFunc("DELETE /v1/pods/{name}", func(w http.ResponseWriter, r *http.Request) {
		err := s.Delete(r.PathValue("name"))
		if IsNotFound(err) {
			httpError(w, http.StatusNotFound, "%v", err)
			return
		}
		if err != nil {
			httpError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.Nodes()) })
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		ev := s.Events(since)
		if ev == nil {
			ev = []orch.Event{}
		}
		writeJSON(w, ev)
	})
	mux.HandleFunc("POST /v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var hb orch.Heartbeat
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&hb); err != nil || hb.Node == "" {
			httpError(w, http.StatusBadRequest, "bad heartbeat: %v", err)
			return
		}
		writeJSON(w, s.Heartbeat(hb))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, args...)})
}

// HTTPAgents talks to node agents over their HTTP API.
type HTTPAgents struct {
	Client *http.Client
}

// NewHTTPAgents returns a client with sensible timeouts.
func NewHTTPAgents() *HTTPAgents {
	return &HTTPAgents{Client: &http.Client{Timeout: 30 * time.Second}}
}

// Run asks the agent at addr to start a pod generation.
func (a *HTTPAgents) Run(ctx context.Context, addr string, req orch.RunRequest) error {
	body, _ := json.Marshal(req)
	return a.do(ctx, http.MethodPost, addr+"/v1/containers", body)
}

// Stop asks the agent at addr to stop and remove a pod generation.
func (a *HTTPAgents) Stop(ctx context.Context, addr, pod string, gen int64) error {
	return a.do(ctx, http.MethodDelete, fmt.Sprintf("%s/v1/containers/%s/%d", addr, pod, gen), nil)
}

func (a *HTTPAgents) do(ctx context.Context, method, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}
