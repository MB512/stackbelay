package jobs

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// crockford is the Crockford base32 alphabet used by ULIDs (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a new job id: a ULID (26 characters, Crockford base32) made
// of the creation time in milliseconds (48 bits) and 80 random bits. Ids
// sort by creation time, which keeps job directories and lists in order.
func NewID(now time.Time) (string, error) {
	var b [16]byte
	ms := uint64(now.UnixMilli()) //nolint:gosec // G115: times before 1970 are not used
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("generating job id: %w", err)
	}
	// 128 bits -> 26 base32 digits; the first digit holds the top 3 bits.
	var out [26]byte
	var acc uint64
	bits := 0
	pos := 25
	for i := 15; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[pos] = crockford[acc&31]
			pos--
			acc >>= 5
			bits -= 5
		}
	}
	out[0] = crockford[acc&31] // remaining 3 bits
	return string(out[:]), nil
}

// IDTime returns the creation time encoded in a job id (millisecond
// precision). ok is false for invalid ids.
func IDTime(id string) (t time.Time, ok bool) {
	if !ValidJobID(id) {
		return time.Time{}, false
	}
	var ms uint64
	for _, c := range id[:10] { // 10 characters = 50 bits, the top 2 are zero
		ms = ms<<5 | uint64(strings.IndexRune(crockford, c)) //nolint:gosec // G115: index of a valid character
	}
	return time.UnixMilli(int64(ms)).UTC(), true //nolint:gosec // G115: 48-bit value
}
