package edge

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// crockford is Crockford base32, which excludes I, L, O, and U so that a nonce
// read aloud or transcribed from a log cannot be ambiguous.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewNonce returns a ULID: 48 bits of millisecond timestamp followed by 80 bits
// of randomness, rendered as 26 Crockford base32 characters.
//
// The value is the object key, the replay identifier, and the idempotency key
// at once. Making one value serve all three is what makes a retried submission
// a no-op by construction rather than by a comparison someone has to remember
// to write.
func NewNonce(now time.Time) (string, error) {
	var raw [16]byte
	ms := uint64(now.UTC().UnixMilli())
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", fmt.Errorf("generate nonce randomness: %w", err)
	}

	// 16 bytes is 128 bits; the standard ULID encoding pads to 130 bits so the
	// leading character encodes only the top 3 bits of the timestamp.
	out := make([]byte, 26)
	out[0] = crockford[(raw[0]&224)>>5]
	out[1] = crockford[raw[0]&31]
	var bits, acc uint
	idx := 2
	for _, b := range raw[1:] {
		acc = acc<<8 | uint(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[idx] = crockford[(acc>>bits)&31]
			idx++
		}
	}
	return string(out), nil
}

// NonceTime recovers the submission time encoded in a nonce's leading 48 bits.
//
// This lets a waiter compute elapsed time from the submission itself rather
// than from when the waiting process happened to start. Without it, a wait that
// times out and is resumed restarts its clock, so elapsed never accumulates and
// an escalation ceiling measured in hours is unreachable by any single
// invocation.
func NonceTime(nonce string) (time.Time, error) {
	if len(nonce) != 26 {
		return time.Time{}, fmt.Errorf("nonce %q is %d characters, want 26", nonce, len(nonce))
	}
	var ms uint64
	for i := range 10 { // the first 10 characters carry the 48-bit timestamp
		idx := strings.IndexByte(crockford, nonce[i])
		if idx < 0 {
			return time.Time{}, fmt.Errorf("nonce %q contains %q, which is not Crockford base32",
				nonce, nonce[i])
		}
		ms = ms<<5 | uint64(idx)
	}
	// Ten base32 characters carry 50 bits; the timestamp is the low 48.
	ms &= (1 << 48) - 1
	return time.UnixMilli(int64(ms)).UTC(), nil
}
