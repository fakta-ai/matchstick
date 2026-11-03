package naming

import (
	"strings"
	"testing"
	"time"
)

func TestNewRoundTrips(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	name := New("ci", "build", at)
	if len(name) != len("ci-build-")+timeLength+randomLength {
		t.Fatalf("unexpected length: %q", name)
	}
	created, err := Created("ci", "build", name)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Equal(at) {
		t.Fatalf("created = %v, want %v", created, at)
	}
}

func TestNewFormat(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for range 50 {
		name := New("ci", "build", at)
		suffix := strings.TrimPrefix(name, "ci-build-")
		if suffix[:timeLength] != "0tnxdc0" {
			t.Fatalf("time part of %q, want 0tnxdc0", name)
		}
		for _, c := range suffix[timeLength:] {
			if !strings.ContainsRune(alphabet, c) {
				t.Fatalf("random part of %q has %q", name, c)
			}
		}
	}
}

func TestNamesSplitOneWayOnly(t *testing.T) {
	// "ci" + "build-linux" must not be read as instance "ci-build" with pool "linux", nor as pool "build".
	name := New("ci", "build-linux", time.Now())
	if _, err := Created("ci", "build-linux", name); err != nil {
		t.Fatal(err)
	}
	for _, owner := range [][2]string{{"ci-build", "linux"}, {"ci", "build"}, {"ci", "linux"}} {
		if _, err := Created(owner[0], owner[1], name); err == nil {
			t.Errorf("%q read as instance %q, pool %q", name, owner[0], owner[1])
		}
	}
	if ValidInstance("ci-build", 10) {
		t.Error("an instance name with a hyphen is valid")
	}
}

func TestCreatedRejectsOtherNames(t *testing.T) {
	other := New("ci", "build-large", time.Now())
	for _, name := range []string{
		"",
		"ci-build-",
		"ci-build-0000000ab",           // too short
		"ci-build-0000000abcde",        // too long
		"ci-build-0000000ABCD",         // upper case
		"other-build-0000000abcd",      // another instance
		"ci-test-0000000abcd",          // another pool
		other,                          // a pool whose name starts with this pool's
		"ci-build-zzzzzzzzzzzzzzzzzzz", // overflows nothing but has the wrong length
	} {
		if _, err := Created("ci", "build", name); err == nil {
			t.Errorf("Created(%q) succeeded", name)
		}
	}
}

func TestValidNames(t *testing.T) {
	for s, want := range map[string]bool{
		"build": true, "build-2": true, "b": true, "build-linux-x64": true,
		"": false, "Build": false, "-build": false, "build-": false, "2build": false, "build_x": false,
		"averyveryverylongpoolname": false,
	} {
		if got := ValidPool(s, 20); got != want {
			t.Errorf("ValidPool(%q) = %v, want %v", s, got, want)
		}
	}
	for s, want := range map[string]bool{"ci": true, "ci2": true, "ci-a": false, "Ci": false, "2ci": false, "": false} {
		if got := ValidInstance(s, 10); got != want {
			t.Errorf("ValidInstance(%q) = %v, want %v", s, got, want)
		}
	}
}
