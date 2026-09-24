package domain

import (
	"testing"
	"time"
)

// FuzzDecodeCursor feeds the watch cursor decoder arbitrary strings: a
// client hands these back verbatim, so nothing it can send may panic, and
// whatever decodes must encode to something that decodes the same.
func FuzzDecodeCursor(f *testing.F) {
	at := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	f.Add(encodeCursor(position{XMin: 7, At: at}))
	f.Add(encodeCursor(position{XMin: 7, At: at, Seen: []seenEvent{{TxID: 7, ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}}}))
	f.Add("v1.")
	f.Add("v1.e30")
	f.Add("")
	f.Fuzz(func(t *testing.T, cursor string) {
		p, err := decodeCursor(cursor)
		if err != nil {
			return
		}
		again, err := decodeCursor(encodeCursor(p))
		if err != nil {
			t.Fatalf("re-encoded cursor does not decode: %v", err)
		}
		if again.XMin != p.XMin || !again.At.Equal(p.At) || len(again.Seen) != len(p.Seen) {
			t.Fatalf("cursor changed through a round trip: %+v became %+v", p, again)
		}
	})
}
