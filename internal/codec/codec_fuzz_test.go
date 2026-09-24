package codec_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/codec"
)

// FuzzUnmarshal feeds the wire codec arbitrary bytes as a request body.
// Nothing a client sends may panic, and any body it accepts must survive
// a round trip through Marshal unchanged.
func FuzzUnmarshal(f *testing.F) {
	seed, err := codec.Marshal(&skiftv1.Node{Id: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Title: "Seed", Labels: []string{"a", "b"}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"unknown": 1}`))
	f.Add([]byte(`{"title": ["not", "a", "string"]}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var n skiftv1.Node
		if err := codec.Unmarshal(raw, &n); err != nil {
			return
		}
		out, err := codec.Marshal(&n)
		if err != nil {
			t.Fatalf("an accepted message does not marshal: %v", err)
		}
		var again skiftv1.Node
		if err := codec.Unmarshal(out, &again); err != nil {
			t.Fatalf("a marshalled message does not unmarshal: %v", err)
		}
		if !proto.Equal(&n, &again) {
			t.Fatalf("message changed through a round trip:\n%s\n%s", raw, out)
		}
	})
}
