// Package naming builds and reads the names matchstick gives machines and their runner registrations.
//
// A name is <instance>-<pool>-<created><random>: the installation's name, the pool's name, the creation time in
// seconds since the Unix epoch in base 36 (7 characters, zero-padded) and 4 random base-36 characters. The name alone
// tells who owns a machine or registration, which pool it serves and when it was made.
package naming

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	timeLength   = 7
	randomLength = 4
	alphabet     = "0123456789abcdefghijklmnopqrstuvwxyz"
)

// identifier is what an instance or pool name may be: lower-case letters, digits and inner hyphens, starting with a
// letter. Names end up in VM names, host names and runner names, which all accept this set.
var identifier = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// ValidIdentifier reports whether s may be used as an instance or pool name of at most maxLength characters.
func ValidIdentifier(s string, maxLength int) bool {
	return len(s) <= maxLength && identifier.MatchString(s)
}

// Prefix returns the part every name of this instance's pool starts with.
func Prefix(instance, pool string) string {
	return instance + "-" + pool + "-"
}

// New returns a fresh name for a machine of the pool, created at t.
func New(instance, pool string, t time.Time) string {
	created := strconv.FormatInt(t.Unix(), 36)
	if len(created) < timeLength {
		created = strings.Repeat("0", timeLength-len(created)) + created
	}
	random := make([]byte, randomLength)
	for i := range random {
		random[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return Prefix(instance, pool) + created + string(random)
}

// Created returns the creation time recorded in a name of the pool. It fails for a name the pool does not own.
func Created(instance, pool, name string) (time.Time, error) {
	suffix, ok := strings.CutPrefix(name, Prefix(instance, pool))
	if !ok || len(suffix) != timeLength+randomLength {
		return time.Time{}, fmt.Errorf("%q is not a name of pool %q", name, pool)
	}
	for _, c := range suffix {
		if !strings.ContainsRune(alphabet, c) {
			return time.Time{}, fmt.Errorf("%q is not a name of pool %q", name, pool)
		}
	}
	seconds, err := strconv.ParseInt(suffix[:timeLength], 36, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a name of pool %q: %w", name, pool, err)
	}
	return time.Unix(seconds, 0).UTC(), nil
}
