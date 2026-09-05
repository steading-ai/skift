package client

import (
	"crypto/rand"
	"time"
)

// crockford is the ULID alphabet: base32 without I, L, O, and U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewRequestID mints an idempotency key for one write: a ULID, so that keys
// sort by the time the request was made and two clients never collide. The
// server treats the key as opaque; the layout is for the humans reading a
// log.
func NewRequestID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err)
	}
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
