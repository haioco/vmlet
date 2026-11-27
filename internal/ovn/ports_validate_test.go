package ovn

import "testing"

func TestValidateSubnetIPAccepts(t *testing.T) {
	cases := []struct{ in string }{{"10.0.0.5"}, {"10.0.0.254"}}
	for _, tc := range cases {
		if err := ValidateSubnetIP("10.0.0.0/24", "10.0.0.1", tc.in, nil); err != nil {
			t.Errorf("ValidateSubnetIP(%s) = %v, want nil", tc.in, err)
		}
	}
}

func TestValidateSubnetIPRejects(t *testing.T) {
	cases := []struct {
		name, subnet, gateway, ip string
		inUse                     []string
	}{
		{"malformed", "10.0.0.0/24", "10.0.0.1", "not-an-ip", nil},
		{"outside", "10.0.0.0/24", "10.0.0.1", "10.0.1.5", nil},
		{"gateway", "10.0.0.0/24", "10.0.0.1", "10.0.0.1", nil},
		{"in use", "10.0.0.0/24", "10.0.0.1", "10.0.0.7", []string{"10.0.0.7"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSubnetIP(tc.subnet, tc.gateway, tc.ip, tc.inUse); err == nil {
				t.Errorf("ValidateSubnetIP accepted %q", tc.ip)
			}
		})
	}
}
