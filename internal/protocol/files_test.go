package protocol

import (
	"encoding/json"
	"testing"
)

func TestFileControlWireShape(t *testing.T) {
	for _, tc := range []struct {
		c    Control
		want string
	}{
		{Control{T: ControlFileOffer, File: &File{ID: "a1", Name: "r.pdf", Size: 12}},
			`{"t":"file-offer","file":{"id":"a1","name":"r.pdf","size":12}}`},
		{Control{T: ControlFileFetch, File: &File{ID: "a1"}}, `{"t":"file-fetch","file":{"id":"a1"}}`},
		{Control{T: ControlFileSaved, File: &File{ID: "u", Name: "r (1).pdf"}},
			`{"t":"file-saved","file":{"id":"u","name":"r (1).pdf"}}`},
	} {
		b, err := json.Marshal(tc.c)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("%s marshals as %s, want %s", tc.c.T, b, tc.want)
		}
	}
}
