package migration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFlagsSubset(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"x y z", "x y z", true},
		{"x y", "x y z w", true},
		{"x y w", "x y z", false},
		{"", "anything", true},
		{"x", "", false},
	}
	for _, c := range cases {
		if got := flagsSubset(c.a, c.b); got != c.want {
			t.Errorf("flagsSubset(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareNodes(t *testing.T) {
	src := &NodeInfo{FirecrackerVer: "v1.13.1", CPUVendor: "GenuineIntel", CPUModel: "A", CPUFlags: "fpu msr", TotalMemMB: 1}
	same := &NodeInfo{FirecrackerVer: "v1.13.1", CPUVendor: "GenuineIntel", CPUModel: "A", CPUFlags: "fpu msr", ZFSPool: "default", ZFSDataset: "default/x", ZFSAvailBytes: 1, TotalMemMB: 1}
	if r := CompareNodes(src, same); !r.OK || len(r.Warnings) != 0 {
		t.Errorf("identical nodes: OK=%v warnings=%v", r.OK, r.Warnings)
	}

	newerCPU := &NodeInfo{FirecrackerVer: "v1.13.1", CPUVendor: "GenuineIntel", CPUModel: "B", CPUFlags: "fpu msr avx2", ZFSPool: "default", ZFSDataset: "default/x", ZFSAvailBytes: 1, TotalMemMB: 1}
	if r := CompareNodes(src, newerCPU); !r.OK || len(r.Warnings) != 1 {
		t.Errorf("richer target CPU should warn, not fail: %+v", r)
	}

	poorerCPU := &NodeInfo{FirecrackerVer: "v1.13.1", CPUVendor: "GenuineIntel", CPUModel: "B", CPUFlags: "fpu", ZFSPool: "default", ZFSDataset: "default/x", ZFSAvailBytes: 1, TotalMemMB: 1}
	if r := CompareNodes(src, poorerCPU); r.OK {
		t.Errorf("poorer target CPU must fail preflight")
	}

	verMismatch := &NodeInfo{FirecrackerVer: "v1.10.1", CPUVendor: "GenuineIntel", CPUModel: "A", CPUFlags: "fpu msr", ZFSPool: "default", ZFSDataset: "default/x", ZFSAvailBytes: 1, TotalMemMB: 1}
	if r := CompareNodes(src, verMismatch); r.OK || !contains(r.Errors, "Firecracker version mismatch") {
		t.Errorf("version mismatch must fail: %+v", r)
	}

	noZFS := &NodeInfo{FirecrackerVer: "v1.13.1", CPUVendor: "GenuineIntel", CPUModel: "A", CPUFlags: "fpu msr", TotalMemMB: 1}
	if r := CompareNodes(src, noZFS); r.OK {
		t.Errorf("target without ZFS must fail preflight")
	}
}

func contains(ss []string, sub string) bool {
	for _, s := range ss {
		if len(s) >= len(sub) && s[:len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestMigSnapName(t *testing.T) {
	if got := migSnapName("base", 7); got != "mig-base-7" {
		t.Errorf("migSnapName = %q", got)
	}
}

func TestWriteRangeFullThenDelta(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mem.snap")

	// full ship at offset 0 truncates
	n, err := writeRange(p, 0, bytes.NewReader(bytes.Repeat([]byte{0xAB}, 1024)))
	if err != nil || n != 1024 {
		t.Fatalf("full write: n=%d err=%v", n, err)
	}
	// delta ship appends at offset 1024
	n, err = writeRange(p, 1024, bytes.NewReader(bytes.Repeat([]byte{0xCD}, 512)))
	if err != nil || n != 512 {
		t.Fatalf("delta write: n=%d err=%v", n, err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1536 {
		t.Fatalf("assembled size = %d, want 1536", len(got))
	}
	if got[0] != 0xAB || got[1024] != 0xCD {
		t.Fatal("offset patching corrupted data")
	}
}

func TestManifestRoundTrip(t *testing.T) {
	m := &Manifest{Version: ManifestVersion, DiskFormat: "zfs-send", DiskBytes: 42}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var m2 Manifest
	if err := json.Unmarshal(b, &m2); err != nil {
		t.Fatal(err)
	}
	if m2.Version != ManifestVersion || m2.DiskBytes != 42 {
		t.Fatalf("round trip lost fields: %+v", m2)
	}
}

func TestCountingReader(t *testing.T) {
	c := &countingReader{r: bytes.NewReader(bytes.Repeat([]byte{1}, 4096))}
	buf := make([]byte, 1024)
	var total int64
	for {
		n, err := c.Read(buf)
		total += int64(n)
		if err != nil {
			break
		}
	}
	if total != 4096 {
		t.Fatalf("counted %d, want 4096", total)
	}
}

func TestSnapshotClientPaths(t *testing.T) {
	// The client only builds requests; assert the param JSON shape.
	p := &CreateSnapshotParams{SnapshotType: "Full", SnapshotPath: "/a", MemFilePath: "/b"}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["snapshot_type"] != "Full" || m["snapshot_path"] != "/a" || m["mem_file_path"] != "/b" {
		t.Fatalf("bad snapshot params JSON: %s", b)
	}
	lp := &LoadSnapshotParams{SnapshotPath: "/a", ResumeVM: true}
	lp.MemBackend.BackendPath = "/b"
	lp.MemBackend.BackendType = "File"
	lp.NetworkOverrides = []NetworkOverride{{IfaceID: "eth0", HostDevName: "tap-x"}}
	b, _ = json.Marshal(lp)
	if !bytes.Contains(b, []byte(`"resume_vm":true`)) ||
		!bytes.Contains(b, []byte(`"backend_type":"File"`)) ||
		!bytes.Contains(b, []byte(`"host_dev_name":"tap-x"`)) {
		t.Fatalf("bad load params JSON: %s", b)
	}
}

func TestDeleteExportRejectsTraversal(t *testing.T) {
	s := &Service{hooks: &Hooks{VMWorkDir: func(string) string { return t.TempDir() }}}
	if err := s.DeleteExport("../evil"); err == nil {
		t.Fatal("path traversal export name must be rejected")
	}
	if err := s.DeleteExport("/abs"); err == nil {
		t.Fatal("absolute export name must be rejected")
	}
}

func TestNewServiceStoresTokenFn(t *testing.T) {
	svc, err := NewService(&Hooks{
		VMWorkDir: func(string) string { return t.TempDir() },
	}, func() string { return "secret" })
	if err != nil {
		t.Skipf("node info unavailable in test env: %v", err)
	}
	if svc.token() != "secret" {
		t.Fatalf("token() = %q, want %q — constructor must store tokenFn", svc.token(), "secret")
	}
}

// TestSendFullStreamsStdout guards the regression where CombinedOutput
// conflicted with a preset Stdout and zfs send never ran.
func TestSendFullStreamsStdout(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "zfs")
	script := "#!/bin/sh\n# ignore args; emit a known payload to stdout\necho -n 'zfs-stream-payload'\n"
	if err := os.WriteFile(stub, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	z := &ZFSOps{Dataset: "test/ds"}
	var buf bytes.Buffer
	if err := z.SendFull("vm-1", "snap", &buf); err != nil {
		t.Fatalf("SendFull: %v", err)
	}
	if buf.String() != "zfs-stream-payload" {
		t.Fatalf("stdout wiring broken, got %q", buf.String())
	}
	if err := z.SendIncremental("vm-1", "a", "b", &buf); err != nil {
		t.Fatalf("SendIncremental: %v", err)
	}
	if buf.String() != "zfs-stream-payloadzfs-stream-payload" {
		t.Fatalf("incremental stdout wiring broken, got %q", buf.String())
	}
}

func TestWriteExtentsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.snap")
	dst := filepath.Join(dir, "dst.snap")

	// Sparse source: page 0 and a page at 1 MiB are data, everything else
	// is a hole — the shape a diff snapshot has.
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("HEAD-DATA"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xAB}, 4096), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(2 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Pre-create the destination at full size (what the full mem ship does).
	if err := os.WriteFile(dst, make([]byte, 2<<20), 0644); err != nil {
		t.Fatal(err)
	}

	// Producer: frame the data extents, exactly like shipMemExtents.
	sf, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()
	var buf bytes.Buffer
	if err := writeMemExtents(sf, 2<<20, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("no extents framed")
	}
	if buf.Len() >= 2<<20 {
		t.Fatalf("extent stream is not sparse: %d bytes", buf.Len())
	}

	// Consumer: apply by offset.
	n, extents, err := writeExtents(dst, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if n <= 0 || extents == 0 {
		t.Fatalf("writeExtents applied %d bytes in %d extents", n, extents)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2<<20 {
		t.Fatalf("destination size %d, want %d", len(got), 2<<20)
	}
	if string(got[:9]) != "HEAD-DATA" {
		t.Fatalf("low-offset page not patched: %q", got[:9])
	}
	for i := 0; i < 4096; i++ {
		if got[1<<20+i] != 0xAB {
			t.Fatalf("high-offset page not patched at %d", i)
		}
	}
	// A hole stays zero.
	if got[12345] != 0 {
		t.Fatalf("hole corrupted at 12345: %d", got[12345])
	}
}

func TestWriteExtentsRejectsNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.snap")
	var buf bytes.Buffer
	// offset = 2^63 (negative as int64), length = 1
	buf.Write([]byte{0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	if _, _, err := writeExtents(path, &buf); err == nil {
		t.Fatal("expected error for negative offset")
	}
}

func TestCheckCapacity(t *testing.T) {
	dst := &NodeInfo{MemAvailableMB: 12000, ZFSAvailBytes: 100 << 30}
	if memOK, diskOK, errs, _ := CheckCapacity(dst, 8192, 61440); !memOK || !diskOK || len(errs) != 0 {
		t.Fatalf("expected fit, got mem=%v disk=%v errs=%v", memOK, diskOK, errs)
	}
	tight := &NodeInfo{MemAvailableMB: 9000, ZFSAvailBytes: 100 << 30}
	if memOK, _, _, _ := CheckCapacity(tight, 8192, 61440); memOK {
		t.Fatal("expected memory capacity failure")
	}
	small := &NodeInfo{MemAvailableMB: 64000, ZFSAvailBytes: 1 << 30}
	if _, diskOK, _, _ := CheckCapacity(small, 8192, 61440); diskOK {
		t.Fatal("expected disk capacity failure")
	}
}
