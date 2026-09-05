package domain

import (
	"crypto/rand"
	"time"
)

// crockford is the ULID alphabet: base32 without I, L, O, and U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID mints an identifier in canonical ULID text: 48 bits of
// millisecond time then 80 random bits, encoded as 26 characters. Time
// leads so that ids sort by creation, which the event stream relies on;
// nothing else reads the layout back.
func newULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err)
	}

	// 128 bits packed into 26 five-bit groups, most significant first. The
	// leading group holds two bits of padding, so it is at most 7.
	var out [26]byte
	var acc uint64
	bits := 0
	pos := len(out) - 1
	for i := len(b) - 1; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[pos] = crockford[acc&31]
			pos--
			acc >>= 5
			bits -= 5
		}
	}
	out[0] = crockford[acc&31]
	return string(out[:])
}
