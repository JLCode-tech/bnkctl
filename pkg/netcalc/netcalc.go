// Package netcalc provides pure-Go CIDR and IP address calculations used across
// cloud underlay provisioners, BNK overlay templates, and lab scenarios.
package netcalc

import (
	"fmt"
	"math"
	"math/big"
	"net"
)

// PrefixLen returns the prefix length (e.g. 24 for "10.0.1.0/24") or 24 on error.
func PrefixLen(cidr string) int {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 24
	}
	ones, _ := ipNet.Mask.Size()
	return ones
}

// CIDRPrefixLen returns the prefix-length portion of cidr, returning an error on invalid input.
func CIDRPrefixLen(cidr string) (int, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, fmt.Errorf("netcalc: parse cidr %q: %w", cidr, err)
	}
	ones, _ := ipnet.Mask.Size()
	return ones, nil
}

// CIDRAddr returns the network-address portion of cidr (e.g. "10.66.0.0/16" -> "10.66.0.0").
func CIDRAddr(cidr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("netcalc: parse cidr %q: %w", cidr, err)
	}
	return ipnet.IP.String(), nil
}

// Nth returns the IPv4 address at network-offset n inside cidr (network address + n).
// Returns an error if cidr does not parse, is not IPv4, or if the resulting address
// falls outside the network.
func Nth(cidr string, n int) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("netcalc: parse cidr %q: %w", cidr, err)
	}
	base4 := ipnet.IP.To4()
	if base4 == nil {
		return "", fmt.Errorf("netcalc: %q is not an IPv4 network", cidr)
	}
	if n < 0 {
		return "", fmt.Errorf("netcalc: negative offset %d", n)
	}
	base := uint32(base4[0])<<24 | uint32(base4[1])<<16 | uint32(base4[2])<<8 | uint32(base4[3])
	if uint64(n) > uint64(math.MaxUint32)-uint64(base) {
		return "", fmt.Errorf("netcalc: offset %d overflows IPv4 address space from %s", n, ipnet)
	}
	// #nosec G115 -- n is non-negative and bounded to fit within uint32 above
	addr := base + uint32(n)
	result := net.IPv4(byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr)) // #nosec G115 -- addr shift masked to byte
	if !ipnet.Contains(result) {
		return "", fmt.Errorf("netcalc: %s is outside %s", result, ipnet)
	}
	return result.String(), nil
}

// IPAtOffset returns the IPv4 address at offset n inside cidr, or "" on error.
func IPAtOffset(cidr string, offset int) string {
	ip, err := Nth(cidr, offset)
	if err != nil {
		return ""
	}
	return ip
}

// GatewayIP returns the first usable host (.1) inside cidr, or "" on error.
func GatewayIP(cidr string) string {
	return IPAtOffset(cidr, 1)
}

// IPv6PrefixLen returns the IPv6 prefix length or 64 on error.
func IPv6PrefixLen(cidr string) int {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 64
	}
	ones, _ := ipNet.Mask.Size()
	return ones
}

// IPv6AtOffset returns the IPv6 address at the given host offset inside cidr, or "" on error.
func IPv6AtOffset(cidr string, offset int64) string {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ip16 := ipNet.IP.To16()
	if ip16 == nil || ipNet.IP.To4() != nil {
		return ""
	}
	baseInt := new(big.Int).SetBytes(ip16)
	baseInt.Add(baseInt, big.NewInt(offset))
	b := baseInt.Bytes()
	out := make(net.IP, 16)
	if len(b) <= 16 {
		copy(out[16-len(b):], b)
	}
	if !ipNet.Contains(out) {
		return ""
	}
	return out.String()
}

// MACForIP returns the GCP virtio-derived MAC address for an internal IPv4 address:
// "42:01:" + hex encoding of each octet (e.g. "10.31.0.49" -> "42:01:0a:1f:00:31").
func MACForIP(ip string) (string, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "", fmt.Errorf("netcalc: not an IP address: %q", ip)
	}
	v4 := parsed.To4()
	if v4 == nil {
		return "", fmt.Errorf("netcalc: not an IPv4 address: %q", ip)
	}
	return fmt.Sprintf("42:01:%02x:%02x:%02x:%02x", v4[0], v4[1], v4[2], v4[3]), nil
}

// InCIDR reports whether ip falls inside cidr.
func InCIDR(ip, cidr string) (bool, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false, fmt.Errorf("netcalc: not an IP address: %q", ip)
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false, fmt.Errorf("netcalc: parse cidr %q: %w", cidr, err)
	}
	return ipnet.Contains(parsed), nil
}
