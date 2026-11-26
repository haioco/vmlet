package ovn

import "testing"

func TestLogicalPortAddresses(t *testing.T) {
	cases := []struct {
		name string
		mac  string
		ip   string
		want []string
	}{
		{"mac and ip", "02:fc:00:00:00:01", "10.0.0.10", []string{"02:fc:00:00:00:01 10.0.0.10"}},
		{"mac only", "02:fc:00:00:00:01", "", []string{"02:fc:00:00:00:01"}},
		{"no mac", "", "10.0.0.10", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LogicalPortAddresses(tc.mac, tc.ip)
			if !strSlicesEqual(got, tc.want) {
				t.Errorf("LogicalPortAddresses(%q, %q) = %v, want %v", tc.mac, tc.ip, got, tc.want)
			}
		})
	}
}

func TestSplitLogicalPortAddress(t *testing.T) {
	cases := []struct {
		in      []string
		wantMac string
		wantIP  string
	}{
		{[]string{"02:fc:00:00:00:01 10.0.0.10"}, "02:fc:00:00:00:01", "10.0.0.10"},
		{[]string{"02:fc:00:00:00:01"}, "02:fc:00:00:00:01", ""},
		{[]string{}, "", ""},
		{[]string{""}, "", ""},
	}
	for _, tc := range cases {
		mac, ip := SplitLogicalPortAddress(tc.in)
		if mac != tc.wantMac || ip != tc.wantIP {
			t.Errorf("SplitLogicalPortAddress(%v) = (%q, %q), want (%q, %q)",
				tc.in, mac, ip, tc.wantMac, tc.wantIP)
		}
	}
}

func TestAddressIPs(t *testing.T) {
	got := AddressIPs([]string{"aa:bb 10.0.0.10", "aa:bb", ""})
	want := []string{"10.0.0.10"}
	if !strSlicesEqual(got, want) {
		t.Errorf("AddressIPs = %v, want %v", got, want)
	}
}

func TestAllocateSubnetIP(t *testing.T) {
	cases := []struct {
		name string
		cidr string
		used []string
		want string
	}{
		{"first free", "10.0.0.0/24", nil, "10.0.0.1"},
		{"second free", "10.0.0.0/24", []string{"10.0.0.1"}, "10.0.0.2"},
		{"skips a gap", "10.0.0.0/24", []string{"10.0.0.2"}, "10.0.0.1"},
		{"fills the gap", "10.0.0.0/24", []string{"10.0.0.1", "10.0.0.3"}, "10.0.0.2"},
		{"small subnet", "10.0.0.0/30", nil, "10.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AllocateSubnetIP(tc.cidr, tc.used)
			if err != nil {
				t.Fatalf("AllocateSubnetIP: %v", err)
			}
			if got != tc.want {
				t.Errorf("AllocateSubnetIP(%s) = %q, want %q", tc.cidr, got, tc.want)
			}
		})
	}
}

func TestAllocateSubnetIP_Exhausted(t *testing.T) {
	// /30 has 4 addresses: network, .1, .2, broadcast -> only 2 usable.
	if _, err := AllocateSubnetIP("10.0.0.0/30", []string{"10.0.0.1", "10.0.0.2"}); err == nil {
		t.Fatal("AllocateSubnetIP accepted an exhausted subnet")
	}
}

func TestAllocateSubnetIP_BadCIDR(t *testing.T) {
	if _, err := AllocateSubnetIP("nope", nil); err == nil {
		t.Fatal("AllocateSubnetIP accepted an invalid CIDR")
	}
}

func TestAllocateSubnetIP_Deterministic(t *testing.T) {
	used := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	first, err := AllocateSubnetIP("10.0.0.0/24", used)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := AllocateSubnetIP("10.0.0.0/24", used)
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("AllocateSubnetIP is not deterministic: %q vs %q", again, first)
		}
	}
}

func TestPortSecurityFor(t *testing.T) {
	mac, ip := "02:fc:00:00:00:01", "10.0.0.10"

	t.Run("unset pins the mac only", func(t *testing.T) {
		t.Setenv(EnvAntispoofEnable, "")
		t.Setenv(EnvAntispoofIPEnable, "")
		ps := PortSecurityFor(mac, ip)
		if ps.MACAddress != mac || ps.Ipv4Address != "" {
			t.Errorf("got %+v, want only the MAC", ps)
		}
	})

	t.Run("ip enable pins the address", func(t *testing.T) {
		t.Setenv(EnvAntispoofIPEnable, "true")
		ps := PortSecurityFor(mac, ip)
		if ps.Ipv4Address != ip {
			t.Errorf("got %+v, want the IP pinned", ps)
		}
	})

	t.Run("enable=false pins nothing", func(t *testing.T) {
		t.Setenv(EnvAntispoofEnable, "false")
		t.Setenv(EnvAntispoofIPEnable, "true")
		ps := PortSecurityFor(mac, ip)
		if ps.Ipv4Address != "" {
			t.Errorf("got %+v, want nothing pinned", ps)
		}
	})
}

func TestLogicalPortNameSanitisesVMIDs(t *testing.T) {
	// VM ids arrive from user input, so the port name must always be safe.
	if got := LogicalPortName("my_vm/1"); got != "fc-port-my-vm-1" {
		t.Errorf("LogicalPortName(sanitise) = %q, want fc-port-my-vm-1", got)
	}
	if got := LogicalPortName(""); got != "fc-port-vm-default" {
		t.Errorf("LogicalPortName(empty) = %q, want a stable fallback", got)
	}
}

func TestExportedCIDRWrappers(t *testing.T) {
	ip, ipNet, err := ParseCIDR("10.0.0.0/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	if ip.String() != "10.0.0.0" {
		t.Errorf("ip = %q", ip)
	}
	if got := FirstUsable(ipNet).String(); got != "10.0.0.1" {
		t.Errorf("FirstUsable = %q", got)
	}
	if got := NetmaskToDotted(ipNet); got != "255.255.255.0" {
		t.Errorf("NetmaskToDotted = %q", got)
	}
}
