package network

import (
	"encoding/binary"
	"fmt"
	"net"
)

// nextFree returns the lowest usable address in subnet that is not
// taken. The network address, the gateway and the broadcast address are
// never handed out.
func nextFree(subnet *net.IPNet, gateway net.IP, taken map[string]string) (net.IP, error) {
	base := ipToUint(subnet.IP.To4())
	ones, bits := subnet.Mask.Size()
	size := uint32(1) << uint(bits-ones)
	gw := ipToUint(gateway.To4())
	for off := uint32(1); off < size-1; off++ {
		ip := base + off
		if ip == gw {
			continue
		}
		candidate := uintToIP(ip)
		if _, used := taken[candidate.String()]; !used {
			return candidate, nil
		}
	}
	return nil, fmt.Errorf("no free addresses left in %s", subnet)
}

func ipToUint(ip net.IP) uint32 { return binary.BigEndian.Uint32(ip) }

func uintToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}
