package vqtest

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewClipRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		bg   Background
		w, h int
		segs []Segment
		want string
	}{
		{"odd size", Captured, 641, 360, []Segment{{Still, 1}}, "bad clip size"},
		{"tiny", Synthetic, 32, 32, []Segment{{Still, 1}}, "bad clip size"},
		{"empty segment", Captured, 640, 360, []Segment{{Typing, 0}}, "typing segment of 0 frames"},
		{"bigger than the capture", Captured, 3840, 2160, []Segment{{Still, 1}}, "smaller than 3840x2160"},
		{"unknown background", Background(9), 640, 360, []Segment{{Still, 1}}, "unknown background"},
	} {
		if _, err := NewClip(tc.bg, tc.w, tc.h, tc.segs...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestClipTimeline(t *testing.T) {
	c, err := NewClip(Synthetic, 320, 180, Segment{Still, 3}, Segment{Typing, 7}, Segment{Scroll, 2})
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != 12 {
		t.Fatalf("Len %d", c.Len())
	}
	if k, j := c.At(4); k != Typing || j != 1 {
		t.Fatalf("At(4) = %v, %d", k, j)
	}
	var changed []bool
	for i := range c.Len() {
		changed = append(changed, c.Changed(i))
	}
	want := []bool{true, false, false, true, false, false, true, false, false, true, true, true}
	for i := range want {
		if changed[i] != want[i] {
			t.Fatalf("Changed: %v, want %v", changed, want)
		}
	}
	if (&Clip{}).Len() != 0 {
		t.Fatal("an empty clip has frames")
	}
}

// TestClipDraws checks each kind of segment draws what Changed says and
// draws the same frame the same way twice.
func TestClipDraws(t *testing.T) {
	for _, bg := range []Background{Captured, Synthetic} {
		c, err := NewClip(bg, 640, 360,
			Segment{Still, 2}, Segment{Typing, 6}, Segment{Scroll, 2}, Segment{Drag, 2}, Segment{Video, 2})
		if err != nil {
			t.Fatal(err)
		}
		frames := make([][]byte, c.Len())
		for i := range frames {
			frames[i] = make([]byte, 4*c.W*c.H)
			c.Draw(frames[i], i)
		}
		for i := 1; i < c.Len(); i++ {
			if differs := !bytes.Equal(frames[i], frames[i-1]); differs != c.Changed(i) {
				t.Errorf("background %d frame %d: differs %v, Changed %v", bg, i, differs, c.Changed(i))
			}
		}
		again := make([]byte, 4*c.W*c.H)
		for _, i := range []int{4, 9, 11, 13} {
			c.Draw(again, i)
			if !bytes.Equal(again, frames[i]) {
				t.Errorf("background %d frame %d drew differently twice", bg, i)
			}
		}
	}
}

func TestBounce(t *testing.T) {
	for _, tc := range [][3]int{{5, 10, 5}, {12, 10, 8}, {20, 10, 0}, {-3, 10, 3}, {7, 0, 0}} {
		if got := bounce(tc[0], tc[1]); got != tc[2] {
			t.Errorf("bounce(%d, %d) = %d, want %d", tc[0], tc[1], got, tc[2])
		}
	}
}

func TestKindNames(t *testing.T) {
	if Scroll.String() != "scroll" || Kind(42).String() != "kind(42)" {
		t.Fatalf("%v %v", Scroll, Kind(42))
	}
}
