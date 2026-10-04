package network

import "testing"

func TestParsePortMapping(t *testing.T) {
	good := map[string]PortMapping{
		"8080:80":              {HostPort: 8080, ContainerPort: 80, Proto: "tcp"},
		"53:53/udp":            {HostPort: 53, ContainerPort: 53, Proto: "udp"},
		"127.0.0.1:8443:443":   {HostIP: "127.0.0.1", HostPort: 8443, ContainerPort: 443, Proto: "tcp"},
		"10.0.0.1:9000:90/TCP": {HostIP: "10.0.0.1", HostPort: 9000, ContainerPort: 90, Proto: "tcp"},
	}
	for in, want := range good {
		got, err := ParsePortMapping(in)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "80", "a:80", "80:b", "0:80", "80:70000", "80:80/sctp", "::1:80:80", "1:2:3:4"} {
		if _, err := ParsePortMapping(in); err == nil {
			t.Errorf("%q: parsed, want error", in)
		}
	}
}

func TestConflicts(t *testing.T) {
	a, _ := ParsePortMapping("8080:80")
	b, _ := ParsePortMapping("127.0.0.1:8080:80")
	c, _ := ParsePortMapping("8080:80/udp")
	d, _ := ParsePortMapping("10.0.0.1:8080:80")
	if !a.Conflicts(b) || a.Conflicts(c) || !b.Conflicts(b) || b.Conflicts(d) {
		t.Error("conflict rules wrong")
	}
}
