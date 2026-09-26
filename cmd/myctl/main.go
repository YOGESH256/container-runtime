// Command myctl manages pods on the cluster through the control plane.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"container-runtime/internal/orch"
)

const usage = `usage: myctl [--server URL] <command>

commands:
  apply [-f] FILE...      create or update pods from JSON files (one pod or a list; - for stdin)
  get pods|nodes [-o json]
  describe pod NAME       spec, status and recent events
  delete pod NAME
  events [-f]             the control plane's activity log (-f to follow)

The server defaults to $MYCTL_SERVER or http://127.0.0.1:7070.
`

var server string

func main() {
	fs := flag.NewFlagSet("myctl", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	def := os.Getenv("MYCTL_SERVER")
	if def == "" {
		def = "http://127.0.0.1:7070"
	}
	fs.StringVar(&server, "server", def, "control plane URL")
	fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	var err error
	switch args[0] {
	case "apply":
		err = apply(args[1:])
	case "get":
		err = get(args[1:])
	case "describe":
		err = describe(args[1:])
	case "delete":
		err = del(args[1:])
	case "events":
		err = events(args[1:])
	default:
		fs.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "myctl:", err)
		os.Exit(1)
	}
}

func apply(args []string) error {
	var files []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-f" && i+1 < len(args) {
			i++
		}
		files = append(files, args[i])
	}
	if len(files) == 0 {
		return fmt.Errorf("usage: myctl apply [-f] FILE")
	}
	for _, file := range files {
		var data []byte
		var err error
		if file == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(file)
		}
		if err != nil {
			return err
		}
		var specs []orch.PodSpec
		if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '[' {
			err = json.Unmarshal(data, &specs)
		} else {
			var one orch.PodSpec
			err = json.Unmarshal(data, &one)
			specs = []orch.PodSpec{one}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		for _, spec := range specs {
			body, _ := json.Marshal(spec)
			if err := call(http.MethodPut, "/v1/pods/"+spec.Name, body, nil); err != nil {
				return err
			}
			fmt.Printf("pod/%s applied\n", spec.Name)
		}
	}
	return nil
}

func get(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: myctl get pods|nodes [-o json]")
	}
	asJSON := len(args) >= 3 && args[1] == "-o" && args[2] == "json"
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	defer w.Flush()
	switch args[0] {
	case "pods", "pod":
		var pods []orch.Pod
		if err := call(http.MethodGet, "/v1/pods", nil, &pods); err != nil {
			return err
		}
		if asJSON {
			return printJSON(pods)
		}
		fmt.Fprintln(w, "NAME\tPHASE\tNODE\tIP\tGEN\tRESTARTS\tAGE\tMESSAGE")
		for _, p := range pods {
			st := p.Status
			since := st.RunningAt
			if since.IsZero() {
				since = st.AssignedAt
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n", p.Spec.Name, st.Phase, dash(st.Node), dash(st.IP),
				st.Generation, st.Restarts, age(since), st.Message)
		}
	case "nodes", "node":
		var nodes []orch.Node
		if err := call(http.MethodGet, "/v1/nodes", nil, &nodes); err != nil {
			return err
		}
		if asJSON {
			return printJSON(nodes)
		}
		fmt.Fprintln(w, "NAME\tREADY\tADDRESS\tLAST HEARTBEAT\tCONTAINERS\tMEMORY\tCPUS")
		for _, n := range nodes {
			fmt.Fprintf(w, "%s\t%v\t%s\t%s ago\t%d\t%.1f GB\t%.0f\n", n.Name, n.Ready, n.Address, age(n.LastHeartbeat),
				len(n.Containers), float64(n.Capacity.MemoryBytes)/(1<<30), n.Capacity.CPUs)
		}
	default:
		return fmt.Errorf("unknown resource %q", args[0])
	}
	return nil
}

func describe(args []string) error {
	if len(args) != 2 || args[0] != "pod" {
		return fmt.Errorf("usage: myctl describe pod NAME")
	}
	var pod orch.Pod
	if err := call(http.MethodGet, "/v1/pods/"+args[1], nil, &pod); err != nil {
		return err
	}
	if err := printJSON(pod); err != nil {
		return err
	}
	var evs []orch.Event
	if err := call(http.MethodGet, "/v1/events", nil, &evs); err != nil {
		return err
	}
	fmt.Println("\nEvents:")
	for _, e := range evs {
		if e.Pod == args[1] {
			fmt.Printf("  %s  %s\n", e.Time.Format("15:04:05.0"), e.Message)
		}
	}
	return nil
}

func del(args []string) error {
	if len(args) != 2 || args[0] != "pod" {
		return fmt.Errorf("usage: myctl delete pod NAME")
	}
	if err := call(http.MethodDelete, "/v1/pods/"+args[1], nil, nil); err != nil {
		return err
	}
	fmt.Printf("pod/%s deleted\n", args[1])
	return nil
}

func events(args []string) error {
	follow := len(args) > 0 && args[0] == "-f"
	var seq int64
	for {
		var evs []orch.Event
		if err := call(http.MethodGet, fmt.Sprintf("/v1/events?since=%d", seq), nil, &evs); err != nil {
			return err
		}
		for _, e := range evs {
			seq = e.Seq
			who := e.Pod
			if who == "" {
				who = "node/" + e.Node
			}
			fmt.Printf("%s  %-12s %s\n", e.Time.Format("15:04:05.0"), who, e.Message)
		}
		if !follow {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func call(method, path string, body []byte, out any) error {
	req, err := http.NewRequest(method, strings.TrimSuffix(server, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
