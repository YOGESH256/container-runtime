//go:build linux

// Package network gives each container its own IP address on a host bridge.
//
//	container A          container B
//	  eth0 10.88.0.2       eth0 10.88.0.3
//	    |  veth pair         |  veth pair
//	  vethXXXX             vethYYYY
//	    +----- mycrt0 -------+      bridge, 10.88.0.1, the containers' gateway
//	              |
//	    host routing + NAT (masquerade) --> eth0 --> internet
//
// A veth pair is a virtual cable: packets entering one end leave the other.
// One end is plugged into the bridge, a virtual switch. The other is moved
// into the container's network namespace and renamed eth0. Containers on
// the bridge reach each other directly. Traffic leaving for anywhere else
// is routed by the host and rewritten to the host's own address.
package network

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/YOGESH256/container-runtime/internal/fsync"
	"github.com/YOGESH256/container-runtime/internal/paths"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	bridgeName    = "mycrt0"
	defaultSubnet = "10.88.0.0/16"
	nftTable      = "mycrt"
)

// Network is the host bridge and the subnet containers get addresses from.
type Network struct {
	Bridge  string
	Subnet  *net.IPNet
	Gateway net.IP
}

// Default returns the bridge network, with the subnet taken from
// MYCRT_SUBNET if set.
func Default() (*Network, error) {
	cidr := os.Getenv("MYCRT_SUBNET")
	if cidr == "" {
		cidr = defaultSubnet
	}
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil || subnet.IP.To4() == nil {
		return nil, fmt.Errorf("invalid IPv4 subnet %q", cidr)
	}
	gw := uintToIP(ipToUint(subnet.IP.To4()) + 1)
	return &Network{Bridge: bridgeName, Subnet: subnet, Gateway: gw}, nil
}

// EnsureHost creates the bridge, turns on IP forwarding and installs the
// NAT rule. It is idempotent and cheap, so it runs before every container.
func (n *Network) EnsureHost() error {
	link, err := netlink.LinkByName(n.Bridge)
	fresh := false
	if _, missing := err.(netlink.LinkNotFoundError); missing {
		fresh = true
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: n.Bridge}}
		if err := netlink.LinkAdd(br); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("create bridge %s: %w", n.Bridge, err)
		}
		link, err = netlink.LinkByName(n.Bridge)
	}
	if err != nil {
		return fmt.Errorf("bridge %s: %w", n.Bridge, err)
	}
	gw := &netlink.Addr{IPNet: &net.IPNet{IP: n.Gateway, Mask: n.Subnet.Mask}}
	if err := netlink.AddrAdd(link, gw); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("address bridge: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up bridge: %w", err)
	}
	// Without forwarding the host drops packets that aren't addressed to it.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0); err != nil {
		return fmt.Errorf("enable IP forwarding: %w", err)
	}
	return n.ensureNAT(fresh)
}

// ensureNAT rewrites the source address of container traffic that leaves
// through any interface other than the bridge, so replies come back to the
// host, which maps them back to the container. The whole table is replaced
// in one atomic nft transaction.
//
// Running nft costs tens of milliseconds, so the applied rules are
// remembered in /run (cleared at boot) and skipped when unchanged. A new
// bridge, as after a reboot, always reapplies them.
func (n *Network) ensureNAT(fresh bool) error {
	rules := fmt.Sprintf(`add table ip %[1]s
flush table ip %[1]s
table ip %[1]s {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %[2]s oifname != "%[3]s" masquerade
	}
}
`, nftTable, n.Subnet, n.Bridge)
	const marker = "/run/mycrt/nat.rules"
	if applied, err := os.ReadFile(marker); !fresh && err == nil && string(applied) == rules {
		return nil
	}
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(string(out)))
	}
	os.MkdirAll(filepath.Dir(marker), 0o700)
	return os.WriteFile(marker, []byte(rules), 0o600)
}

// HostVeth is the name of the container's veth end on the host.
func HostVeth(id string) string { return "veth" + id[:8] }

// Attach plugs the container whose init has host PID pid into the bridge
// and configures eth0 with ip. It runs while the container's init is
// blocked waiting for its config, so the network is ready before the
// container's command starts.
func (n *Network) Attach(id string, pid int, ip net.IP) error {
	br, err := netlink.LinkByName(n.Bridge)
	if err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	// Create the pair with its inner end already inside the container's
	// namespace and already named eth0. Moving or renaming a device later
	// costs an extra RCU grace period each, which is slow on small VMs.
	veth := &netlink.Veth{
		LinkAttrs:        netlink.LinkAttrs{Name: HostVeth(id), MasterIndex: br.Attrs().Index, MTU: br.Attrs().MTU},
		PeerName:         "eth0",
		PeerNamespace:    netlink.NsPid(pid),
		PeerHardwareAddr: MACFor(ip),
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("create veth pair: %w", err)
	}
	done := false
	defer func() {
		if !done {
			netlink.LinkDel(veth) // deleting one end removes both
		}
	}()
	if err := netlink.LinkSetUp(veth); err != nil {
		return fmt.Errorf("bring up host veth: %w", err)
	}

	// Configure the container's end from the host, through a netlink
	// socket opened inside the container's network namespace.
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		return fmt.Errorf("open container netns: %w", err)
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return fmt.Errorf("netlink in container: %w", err)
	}
	defer h.Close()
	inner, err := h.LinkByName("eth0")
	if err != nil {
		return err
	}
	if err := h.AddrAdd(inner, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: n.Subnet.Mask}}); err != nil {
		return fmt.Errorf("assign %s: %w", ip, err)
	}
	if err := h.LinkSetUp(inner); err != nil {
		return fmt.Errorf("bring up eth0: %w", err)
	}
	if lo, err := h.LinkByName("lo"); err == nil {
		h.LinkSetUp(lo)
	}
	if err := h.RouteAdd(&netlink.Route{LinkIndex: inner.Attrs().Index, Gw: n.Gateway}); err != nil {
		return fmt.Errorf("default route: %w", err)
	}
	done = true
	return nil
}

// MACFor derives a container's MAC address from its IP, as Docker does.
//
// Addresses are reused: a replacement container often gets the IP its
// predecessor just released. The host's neighbor (ARP) cache still maps
// that IP to the old container's MAC for up to half a minute, so traffic
// to the new container vanishes. With the MAC a function of the IP, the
// cached entry stays correct. 02 marks a locally administered unicast MAC.
func MACFor(ip net.IP) net.HardwareAddr {
	v4 := ip.To4()
	return net.HardwareAddr{0x02, 0x42, v4[0], v4[1], v4[2], v4[3]}
}

// Detach removes the container's veth pair if it still exists. After a
// normal exit this is unnecessary: the kernel deletes the pair together
// with the container's network namespace, in the background. It is for
// cleaning up after a supervisor that crashed.
func (n *Network) Detach(id string) {
	if l, err := netlink.LinkByName(HostVeth(id)); err == nil {
		netlink.LinkDel(l)
	}
}

// Allocate leases a free address to container id. Leases are kept in a
// JSON file under an exclusive lock, because several mycrt processes may
// start containers at once. Leases owned by containers whose directory no
// longer exists are reclaimed on the way.
func (n *Network) Allocate(id string) (net.IP, error) {
	var ip net.IP
	err := n.editLeases(func(leases map[string]string) error {
		for addr, owner := range leases {
			if _, err := os.Stat(paths.Container(owner)); errors.Is(err, os.ErrNotExist) {
				delete(leases, addr)
			}
		}
		var err error
		ip, err = nextFree(n.Subnet, n.Gateway, leases)
		if err == nil {
			leases[ip.String()] = id
		}
		return err
	})
	return ip, err
}

// Release returns container id's address to the pool.
func (n *Network) Release(id string) error {
	return n.editLeases(func(leases map[string]string) error {
		for addr, owner := range leases {
			if owner == id {
				delete(leases, addr)
			}
		}
		return nil
	})
}

func (n *Network) editLeases(edit func(map[string]string) error) error {
	dir := paths.Network()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "leases.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	file := filepath.Join(dir, "leases.json")
	leases := map[string]string{}
	if b, err := os.ReadFile(file); err == nil {
		if err := json.Unmarshal(b, &leases); err != nil {
			fmt.Fprintf(os.Stderr, "mycrt: %s is damaged (%v); rebuilding it from running containers\n", file, err)
			leases = leasesFromContainers()
		}
	}
	if err := edit(leases); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(leases, "", "  ")
	return fsync.WriteFile(file, b, 0o600)
}

// leasesFromContainers recovers the lease table from the containers'
// own state files, which record the address of every running container.
func leasesFromContainers() map[string]string {
	leases := map[string]string{}
	entries, _ := os.ReadDir(paths.Containers())
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(paths.Container(e.Name()), "state.json"))
		if err != nil {
			continue
		}
		var st struct {
			Status string `json:"status"`
			IP     string `json:"ip"`
		}
		if json.Unmarshal(b, &st) == nil && st.Status == "running" && st.IP != "" {
			leases[st.IP] = e.Name()
		}
	}
	return leases
}

// EtcFiles writes the container's /etc/hosts, /etc/hostname and
// /etc/resolv.conf into dir and returns them keyed by their path inside
// the container, ready to be bind-mounted. ip is nil without networking.
func EtcFiles(dir, hostname string, ip net.IP) (map[string]string, error) {
	self := "127.0.1.1"
	if ip != nil {
		self = ip.String()
	}
	content := map[string]string{
		"/etc/hosts":       fmt.Sprintf("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n%s\t%s\n", self, hostname),
		"/etc/hostname":    hostname + "\n",
		"/etc/resolv.conf": resolvConf(),
	}
	files := map[string]string{}
	for target, body := range content {
		host := filepath.Join(dir, filepath.Base(target))
		if err := os.WriteFile(host, []byte(body), 0o644); err != nil {
			return nil, err
		}
		files[target] = host
	}
	return files, nil
}

// resolvConf builds the container's DNS settings from the host's. On
// systemd hosts /etc/resolv.conf points at 127.0.0.53, which is the host's
// own loopback and unreachable from a container, so loopback servers are
// skipped and systemd-resolved's list of upstream servers is read first.
func resolvConf() string {
	for _, p := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		var out []string
		servers := 0
		s := bufio.NewScanner(f)
		for s.Scan() {
			fields := strings.Fields(s.Text())
			if len(fields) < 2 {
				continue
			}
			switch fields[0] {
			case "nameserver":
				if ip := net.ParseIP(fields[1]); ip != nil && !ip.IsLoopback() {
					out = append(out, s.Text())
					servers++
				}
			case "search", "options":
				out = append(out, s.Text())
			}
		}
		f.Close()
		if servers > 0 {
			return strings.Join(out, "\n") + "\n"
		}
	}
	return "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"
}

// PublishPorts makes the container's ports reachable on the host, in a
// table of its own so adding and removing it never touches other
// containers' rules:
//
//	prerouting   traffic arriving from outside: dnat to the container
//	output       traffic from the host itself, including localhost
//	postrouting  hairpin: a container reaching a published port of another
//	             container through the host gets its replies via the host,
//	             and localhost-sourced packets are given a routable source
func (n *Network) PublishPorts(id string, ip net.IP, ports []PortMapping) error {
	if len(ports) == 0 {
		return nil
	}
	// Packets from 127.0.0.1 may leave through the bridge once DNAT
	// rewrote their destination; the kernel drops them as "martian"
	// unless this is set.
	if err := os.WriteFile("/proc/sys/net/ipv4/conf/"+n.Bridge+"/route_localnet", []byte("1"), 0); err != nil {
		return fmt.Errorf("route_localnet: %w", err)
	}
	var pre, out, post strings.Builder
	for _, p := range ports {
		match := fmt.Sprintf("%s dport %d", p.Proto, p.HostPort)
		if p.HostIP != "" {
			match = "ip daddr " + p.HostIP + " " + match
		}
		fmt.Fprintf(&pre, "\t\t%s dnat to %s:%d\n", match, ip, p.ContainerPort)
		fmt.Fprintf(&out, "\t\tfib daddr type local %s dnat to %s:%d\n", match, ip, p.ContainerPort)
		fmt.Fprintf(&post, "\t\tip saddr %s ip daddr %s %s dport %d masquerade\n", n.Subnet, ip, p.Proto, p.ContainerPort)
	}
	rules := fmt.Sprintf(`add table ip %[1]s
delete table ip %[1]s
table ip %[1]s {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
%[2]s	}
	chain output {
		type nat hook output priority dstnat; policy accept;
%[3]s	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr 127.0.0.0/8 oifname "%[4]s" masquerade
%[5]s	}
}
`, portsTable(id), pre.String(), out.String(), n.Bridge, post.String())
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if outb, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(string(outb)))
	}
	return nil
}

// UnpublishPorts removes the container's port table, if any.
func (n *Network) UnpublishPorts(id string) {
	exec.Command("nft", "delete", "table", "ip", portsTable(id)).Run()
}

func portsTable(id string) string { return "mycrt_" + id }
