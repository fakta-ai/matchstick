package naming

import (
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

func TestNewIsUnique(t *testing.T) {
	at := time.Now()
	seen := map[string]bool{}
	for range 100 {
		name := New("ci", "build", at)
		if seen[name] {
			t.Fatalf("duplicate name %q", name)
		}
		seen[name] = true
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

func TestValidIdentifier(t *testing.T) {
	for s, want := range map[string]bool{
		"build": true, "build-2": true, "b": true,
		"": false, "Build": false, "-build": false, "build-": false, "2build": false, "build_x": false,
		"averyveryverylongpoolname": false,
	} {
		if got := ValidIdentifier(s, 20); got != want {
			t.Errorf("ValidIdentifier(%q) = %v, want %v", s, got, want)
		}
	}
}
