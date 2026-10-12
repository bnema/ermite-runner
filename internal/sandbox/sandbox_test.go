// SPDX-License-Identifier: MIT

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zerowrap"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigDefaults(t *testing.T) {
	c, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Isolation != Auto || c.Capacity != 1 || c.UIDBase != 900000 || c.StateDir != DefaultStateDir ||
		c.JobCPUs != 2 || c.JobMemoryMiB != 4096 || c.JobDiskMiB != 64*1024 || c.Network.String() != "10.200.0.0/16" || c.DNSPort != 53 {
		t.Fatalf("%+v", c)
	}
}

func TestLoadConfigValues(t *testing.T) {
	c, err := LoadConfig(env(map[string]string{
		EnvIsolation: "VM", EnvStateDir: "/srv/er", EnvCapacity: "4", EnvImage: "/images/u24",
		EnvJobCPUs: "8", EnvJobMemory: "16G", EnvJobDisk: "100G", EnvUIDBase: "910000",
		EnvNetwork: "10.50.0.0/24", EnvJailerMode: "off", EnvFirecracker: "/x/fc", EnvKernel: "/k",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Isolation != VM || c.Capacity != 4 || c.JobMemoryMiB != 16384 || c.JobDiskMiB != 100*1024 || c.UIDBase != 910000 ||
		!c.JailerOff || c.Network.String() != "10.50.0.0/24" || c.ImageDir() != "/images/u24" || c.KernelPath() != "/k" ||
		c.BaseImagePath() != "/images/u24/base.ext4" || c.OCIImage() != DefaultOCIImage {
		t.Fatalf("%+v", c)
	}
	c, _ = LoadConfig(env(map[string]string{EnvImage: "registry.example/ermite:1"}))
	if c.OCIImage() != "registry.example/ermite:1" || c.ImageDir() != filepath.Join(DefaultStateDir, "image") {
		t.Fatalf("OCI ref misread: %q %q", c.OCIImage(), c.ImageDir())
	}
}

func TestLoadConfigErrors(t *testing.T) {
	for name, m := range map[string]map[string]string{
		"isolation":    {EnvIsolation: "docker"},
		"relative":     {EnvStateDir: "state"},
		"capacity 0":   {EnvCapacity: "0"},
		"capacity abc": {EnvCapacity: "abc"},
		"cpus":         {EnvJobCPUs: "0"},
		"memory":       {EnvJobMemory: "10M"},
		"disk":         {EnvJobDisk: "100M"},
		"uid base":     {EnvUIDBase: "10"},
		"network v6":   {EnvNetwork: "fd00::/64"},
		"network /31":  {EnvNetwork: "10.0.0.0/31"},
		"network few":  {EnvNetwork: "10.0.0.0/30", EnvCapacity: "2"},
		"jailer mode":  {EnvJailerMode: "maybe"},
	} {
		if _, err := LoadConfig(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseSizeMiB(t *testing.T) {
	for in, want := range map[string]int{"4096": 4096, "512M": 512, "4G": 4096, "64GiB": 65536, "1t": 1048576, " 2g ": 2048, "1024MB": 1024} {
		if got, err := ParseSizeMiB(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "-1", "G", "12K", "1x", "99999999999T"} {
		if _, err := ParseSizeMiB(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestResolve(t *testing.T) {
	c, _ := LoadConfig(env(nil))
	s := c.Resolve(JobSpec{JobID: "j", CPUs: 6})
	if s.CPUs != 6 || s.MemoryMiB != 4096 || s.DiskMiB != 64*1024 || s.JobID != "j" {
		t.Fatalf("%+v", s)
	}
}

func TestSlots(t *testing.T) {
	s := NewSlots(2)
	a, _ := s.Acquire()
	b, _ := s.Acquire()
	if a != 0 || b != 1 || s.InUse() != 2 {
		t.Fatal(a, b)
	}
	if _, err := s.Acquire(); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	s.Release(0)
	s.Release(0)
	s.Release(99)
	if c, _ := s.Acquire(); c != 0 {
		t.Fatalf("lowest free slot first, got %d", c)
	}
}

func TestIDs(t *testing.T) {
	id, err := NewID()
	if err != nil || !ValidID(id) {
		t.Fatal(id, err)
	}
	for _, bad := range []string{"", "er", "er12345", "xx12345678", "er1234567g", "er../../etc", "er123456789"} {
		if ValidID(bad) {
			t.Errorf("%q valid", bad)
		}
	}
}

// fakeDriver is a Driver that records its construction.
type fakeDriver struct{ mode Mode }

func (f fakeDriver) Mode() Mode               { return f.mode }
func (fakeDriver) New() (Sandbox, error)      { return nil, nil }
func (fakeDriver) Reap(context.Context) error { return nil }
func (fakeDriver) Close() error               { return nil }

func facts(built *[]Mode) Factories {
	return Factories{
		VM: func(context.Context, Config) (Driver, error) { *built = append(*built, VM); return fakeDriver{VM}, nil },
		Container: func(context.Context, Config) (Driver, error) {
			*built = append(*built, Container)
			return fakeDriver{Container}, nil
		},
	}
}

// goodProber reports a host where every vm prerequisite exists.
func goodProber() Prober {
	return Prober{
		OpenKVM:  func(string) error { return nil },
		LookPath: func(n string) (string, error) { return "/usr/bin/" + n, nil },
		Stat:     func(string) error { return nil },
	}
}

func logCtx(buf *bytes.Buffer) context.Context {
	return zerowrap.WithCtx(context.Background(), zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: buf}))
}

func TestSelectAutoPicksVMWhenEverythingIsThere(t *testing.T) {
	var built []Mode
	var log bytes.Buffer
	cfg, _ := LoadConfig(env(nil))
	d, err := Select(logCtx(&log), cfg, facts(&built), goodProber())
	if err != nil || d.Mode() != VM || len(built) != 1 {
		t.Fatalf("%v %v %v", d, err, built)
	}
	if !strings.Contains(log.String(), `"mode":"vm"`) || !strings.Contains(log.String(), "reason") {
		t.Fatalf("mode and reason must be logged: %s", log.String())
	}
}

// AC4: auto picks container when /dev/kvm is hidden, and says why.
func TestSelectAutoFallsBackWithoutKVM(t *testing.T) {
	var built []Mode
	var log bytes.Buffer
	cfg, _ := LoadConfig(env(nil))
	cfg.KVMPath = filepath.Join(t.TempDir(), "kvm") // hidden: does not exist
	p := goodProber()
	p.OpenKVM = Prober{}.withDefaults().OpenKVM // the real open, on the hidden path
	d, err := Select(logCtx(&log), cfg, facts(&built), p)
	if err != nil || d.Mode() != Container {
		t.Fatalf("%v %v", d, err)
	}
	out := log.String()
	if !strings.Contains(out, `"mode":"container"`) || !strings.Contains(out, "not usable read-write") {
		t.Fatalf("fallback reason not logged: %s", out)
	}
}

func TestSelectAutoFallbackReasons(t *testing.T) {
	cfg, _ := LoadConfig(env(nil))
	cases := map[string]Prober{
		"firecracker not found": {OpenKVM: func(string) error { return nil }, LookPath: func(n string) (string, error) {
			if n == "firecracker" {
				return "", os.ErrNotExist
			}
			return "/x", nil
		}, Stat: func(string) error { return nil }},
		"jailer not found": {OpenKVM: func(string) error { return nil }, LookPath: func(n string) (string, error) {
			if n == "jailer" {
				return "", os.ErrNotExist
			}
			return "/x", nil
		}, Stat: func(string) error { return nil }},
		"guest kernel": {OpenKVM: func(string) error { return nil }, LookPath: goodProber().LookPath, Stat: func(p string) error {
			if strings.HasSuffix(p, "vmlinux") {
				return os.ErrNotExist
			}
			return nil
		}},
		"base image": {OpenKVM: func(string) error { return nil }, LookPath: goodProber().LookPath, Stat: func(p string) error {
			if strings.HasSuffix(p, "base.ext4") {
				return os.ErrNotExist
			}
			return nil
		}},
	}
	for want, p := range cases {
		var built []Mode
		d, err := Select(context.Background(), cfg, facts(&built), p)
		if err != nil || d.Mode() != Container {
			t.Errorf("%s: %v %v", want, d, err)
		}
		if why := VMUnavailable(cfg, p); len(why) != 1 || !strings.Contains(why[0], want) {
			t.Errorf("%s: reasons %v", want, why)
		}
	}
}

func TestSelectExplicitModes(t *testing.T) {
	cfg, _ := LoadConfig(env(map[string]string{EnvIsolation: "vm"}))
	var built []Mode
	bad := Prober{OpenKVM: func(string) error { return os.ErrPermission }, LookPath: goodProber().LookPath, Stat: goodProber().Stat}
	if _, err := Select(context.Background(), cfg, facts(&built), bad); err == nil || !strings.Contains(err.Error(), "kvm") || len(built) != 0 {
		t.Fatalf("vm must fail loudly without KVM and build nothing: %v %v", err, built)
	}
	cfg.Isolation = Container
	d, err := Select(context.Background(), cfg, facts(&built), bad)
	if err != nil || d.Mode() != Container {
		t.Fatalf("container never probes KVM: %v %v", d, err)
	}
	cfg.Isolation = Mode("x")
	if _, err := Select(context.Background(), cfg, facts(&built), bad); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestSelectDoesNotMaskBrokenVM(t *testing.T) {
	cfg, _ := LoadConfig(env(nil))
	boom := errors.New("kernel does not boot")
	f := Factories{
		VM:        func(context.Context, Config) (Driver, error) { return nil, boom },
		Container: func(context.Context, Config) (Driver, error) { t.Fatal("must not fall back silently"); return nil, nil },
	}
	if _, err := Select(context.Background(), cfg, f, goodProber()); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if _, err := Select(context.Background(), cfg, Factories{}, goodProber()); err == nil {
		t.Fatal("missing factory accepted")
	}
}

func TestCheckStatic(t *testing.T) {
	dir := t.TempDir()
	if err := CheckStatic(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
	txt := filepath.Join(dir, "script")
	_ = os.WriteFile(txt, []byte("#!/bin/sh\n"), 0o755)
	if err := CheckStatic(txt); err == nil || !strings.Contains(err.Error(), "CGO_ENABLED=0") {
		t.Fatalf("script accepted: %v", err)
	}
	if err := CheckStatic("/bin/sh"); err == nil {
		t.Skip("/bin/sh is static here")
	}
}
