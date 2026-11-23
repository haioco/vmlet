package cloudinit

import "testing"

func TestSanitizeHostname(t *testing.T) {
	cases := []struct {
		name     string
		fallback string
		want     string
	}{
		// The production failure: a Persian display name reached `hostname(1)`.
		{"تانل وایرگارد", "vm-9f07d6c6", "vm-9f07d6c6"},
		{"آساکد", "vm-9f07d6c6", "vm-9f07d6c6"},
		{"My Server_01.example", "vm-x", "my-server-01-example"},
		{"IRVMLT", "vm-x", "irvmlt"},
		{"  ---my box---  ", "vm-x", "my-box"},
		{"nexus-meet", "vm-x", "nexus-meet"},
		{"", "vm-abc123", "vm-abc123"},
		{"  ", "vm-abc123", "vm-abc123"},
		{"!!!", "My Clone", "my-clone"},
		{"", "", "vmlet-vm"},
	}
	for _, tc := range cases {
		if got := SanitizeHostname(tc.name, tc.fallback); got != tc.want {
			t.Errorf("SanitizeHostname(%q, %q) = %q, want %q", tc.name, tc.fallback, got, tc.want)
		}
	}
}

func TestSanitizeHostnameTruncatesTo63(t *testing.T) {
	long := ""
	for i := 0; i < 100; i++ {
		long += "a"
	}
	got := SanitizeHostname(long, "vm-x")
	if len(got) != 63 {
		t.Fatalf("expected 63 chars, got %d (%q)", len(got), got)
	}
}

func TestSanitizeHostnameNeverEndsWithDashAfterTruncation(t *testing.T) {
	// 62 chars then a separator: truncation must not leave a trailing dash.
	name := ""
	for i := 0; i < 62; i++ {
		name += "a"
	}
	name += " b"
	got := SanitizeHostname(name, "vm-x")
	if got == "" || got[len(got)-1] == '-' {
		t.Fatalf("invalid truncated hostname %q", got)
	}
}
