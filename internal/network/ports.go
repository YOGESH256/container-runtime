package network

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// PortMapping publishes a container port on the host, like docker's -p.
type PortMapping struct {
	HostIP        string `json:"hostIp,omitempty"` // empty: every host address
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Proto         string `json:"proto"` // tcp or udp
}

// ParsePortMapping accepts [HOSTIP:]HOSTPORT:CONTAINERPORT[/PROTO].
func ParsePortMapping(s string) (PortMapping, error) {
	m := PortMapping{Proto: "tcp"}
	spec := s
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		m.Proto, spec = strings.ToLower(spec[i+1:]), spec[:i]
		if m.Proto != "tcp" && m.Proto != "udp" {
			return m, fmt.Errorf("port %q: protocol must be tcp or udp", s)
		}
	}
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 2:
	case 3:
		if net.ParseIP(parts[0]) == nil || net.ParseIP(parts[0]).To4() == nil {
			return m, fmt.Errorf("port %q: host address must be an IPv4 address", s)
		}
		m.HostIP, parts = parts[0], parts[1:]
	default:
		return m, fmt.Errorf("port %q: use [HOSTIP:]HOSTPORT:CONTAINERPORT[/tcp|udp]", s)
	}
	var err error
	if m.HostPort, err = parsePort(parts[0]); err != nil {
		return m, fmt.Errorf("port %q: %v", s, err)
	}
	if m.ContainerPort, err = parsePort(parts[1]); err != nil {
		return m, fmt.Errorf("port %q: %v", s, err)
	}
	return m, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port number", s)
	}
	return n, nil
}

func (m PortMapping) String() string {
	s := fmt.Sprintf("%d:%d/%s", m.HostPort, m.ContainerPort, m.Proto)
	if m.HostIP != "" {
		s = m.HostIP + ":" + s
	}
	return s
}

// Conflicts reports whether two mappings claim the same host port.
func (m PortMapping) Conflicts(o PortMapping) bool {
	if m.Proto != o.Proto || m.HostPort != o.HostPort {
		return false
	}
	return m.HostIP == "" || o.HostIP == "" || m.HostIP == o.HostIP
}
