package netcalc

import "testing"

func TestNth(t *testing.T) {
	tests := []struct {
		cidr string
		n    int
		want string
	}{
		{"10.31.0.48/28", 1, "10.31.0.49"},
		{"10.60.0.0/24", 1, "10.60.0.1"},
		{"10.30.0.0/24", 1, "10.30.0.1"},
		{"10.50.0.0/24", 1, "10.50.0.1"},
	}
	for _, tt := range tests {
		got, err := Nth(tt.cidr, tt.n)
		if err != nil {
			t.Fatalf("Nth(%q, %d): unexpected error: %v", tt.cidr, tt.n, err)
		}
		if got != tt.want {
			t.Errorf("Nth(%q, %d) = %q, want %q", tt.cidr, tt.n, got, tt.want)
		}
	}
}

func TestNth_OutsideNetwork(t *testing.T) {
	if _, err := Nth("10.31.0.48/28", 20); err == nil {
		t.Fatal("expected error for offset outside network, got nil")
	}
	if _, err := Nth("10.30.0.10/24", -1); err == nil {
		t.Fatal("expected error for negative offset, got nil")
	}
	if _, err := Nth("10.30.0.0/24", (1<<32)+1); err == nil {
		t.Fatal("expected error for offset overflowing uint32, got nil")
	}
}

func TestNth_MalformedCIDR(t *testing.T) {
	if _, err := Nth("not-a-cidr", 1); err == nil {
		t.Fatal("expected error for malformed cidr, got nil")
	}
}

func TestMACForIP(t *testing.T) {
	tests := []struct {
		ip   string
		want string
	}{
		{"10.31.0.49", "42:01:0a:1f:00:31"},
		{"10.51.0.49", "42:01:0a:33:00:31"},
		{"0.0.0.0", "42:01:00:00:00:00"},
		{"255.255.255.255", "42:01:ff:ff:ff:ff"},
	}
	for _, tt := range tests {
		got, err := MACForIP(tt.ip)
		if err != nil {
			t.Fatalf("MACForIP(%q): unexpected error: %v", tt.ip, err)
		}
		if got != tt.want {
			t.Errorf("MACForIP(%q) = %q, want %q", tt.ip, got, tt.want)
		}
	}
}

func TestMACForIP_NotAnIP(t *testing.T) {
	if _, err := MACForIP("not-an-ip"); err == nil {
		t.Fatal("expected error for non-IP input, got nil")
	}
}

func TestMACForIP_IPv6Rejected(t *testing.T) {
	if _, err := MACForIP("::1"); err == nil {
		t.Fatal("expected error for IPv6 input, got nil")
	}
}

func TestInCIDR(t *testing.T) {
	tests := []struct {
		ip   string
		cidr string
		want bool
	}{
		{"10.30.0.5", "10.30.0.0/24", true},
		{"10.31.0.5", "10.30.0.0/24", false},
		{"10.0.0.0", "10.0.0.0/8", true},
	}
	for _, tt := range tests {
		got, err := InCIDR(tt.ip, tt.cidr)
		if err != nil {
			t.Fatalf("InCIDR(%q, %q): unexpected error: %v", tt.ip, tt.cidr, err)
		}
		if got != tt.want {
			t.Errorf("InCIDR(%q, %q) = %v, want %v", tt.ip, tt.cidr, got, tt.want)
		}
	}
}

func TestCIDRAddr(t *testing.T) {
	got, err := CIDRAddr("10.66.0.0/16")
	if err != nil {
		t.Fatalf("CIDRAddr: unexpected error: %v", err)
	}
	if got != "10.66.0.0" {
		t.Errorf("CIDRAddr(10.66.0.0/16) = %q, want 10.66.0.0", got)
	}
}

func TestCIDRPrefixLen(t *testing.T) {
	got, err := CIDRPrefixLen("10.66.0.0/16")
	if err != nil {
		t.Fatalf("CIDRPrefixLen: unexpected error: %v", err)
	}
	if got != 16 {
		t.Errorf("CIDRPrefixLen(10.66.0.0/16) = %d, want 16", got)
	}
}

func TestIPv6AtOffset(t *testing.T) {
	got := IPv6AtOffset("fd00:10:60::/64", 100)
	if got != "fd00:10:60::64" {
		t.Errorf("IPv6AtOffset = %q, want fd00:10:60::64", got)
	}
}
