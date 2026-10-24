package vqtest

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// Sent is one access unit the sender put on the wire for a clip frame.
type Sent struct {
	Index int
	Bytes int
	Key   bool
	// QP is the encoder's average quantiser for the frame, 0 when it does
	// not say.
	QP int
}

// Run is everything one pass of a clip produced.
type Run struct {
	Clip *Clip
	FPS  int
	// Start is when frame 0 was captured; frame i at Start + i/FPS.
	Start  time.Time
	Sent   []Sent
	Result Result
}

// Span is one segment's numbers, or the whole clip's.
type Span struct {
	Name       string
	From, To   int // clip frames [From, To)
	Seconds    float64
	SentFrames int
	Keyframes  int // sent
	KbpsSent   float64
	MeanQP     float64
	Shown      int
	FPS        float64
	// MaxFreeze is the longest wait, in a span where the picture changes,
	// between two pictures shown or from the span's start to its first.
	MaxFreeze time.Duration
	// Lag is the mean time from capturing a frame to showing it.
	Lag           time.Duration
	PSNR, MinPSNR float64
	SSIM, MinSSIM float64
	WorstTile     float64 // lowest worst-tile SSIM of any picture
	Last          Score   // the last picture shown in the span
	Requests      map[string]int
}

// Spans splits a run by segment, with the whole clip last.
func (r *Run) Spans() []Span {
	var out []Span
	counts := map[Kind]int{}
	for s, seg := range r.Clip.segs {
		counts[seg.Kind]++
		name := seg.Kind.String()
		if n := countKind(r.Clip.segs, seg.Kind); n > 1 {
			name = fmt.Sprintf("%s %d", name, counts[seg.Kind])
		}
		from := r.Clip.starts[s]
		out = append(out, r.span(name, from, from+seg.Frames, seg.Kind != Still))
	}
	return append(out, r.span("all", 0, r.Clip.Len(), false))
}

func countKind(segs []Segment, k Kind) int {
	n := 0
	for _, s := range segs {
		if s.Kind == k {
			n++
		}
	}
	return n
}

func (r *Run) at(i int) time.Time {
	return r.Start.Add(time.Duration(i) * time.Second / time.Duration(r.FPS))
}

func (r *Run) span(name string, from, to int, moving bool) Span {
	s := Span{Name: name, From: from, To: to, Seconds: float64(to-from) / float64(r.FPS),
		Requests: map[string]int{}, MinPSNR: MaxPSNR, MinSSIM: 1, WorstTile: 1}
	bytes, qps := 0, 0
	for _, f := range r.Sent {
		if f.Index < from || f.Index >= to {
			continue
		}
		s.SentFrames++
		bytes += f.Bytes
		if f.Key {
			s.Keyframes++
		}
		if f.QP > 0 {
			s.MeanQP += float64(f.QP)
			qps++
		}
	}
	if qps > 0 {
		s.MeanQP /= float64(qps)
	}
	s.KbpsSent = float64(8*bytes) / s.Seconds / 1000
	begin, end := r.at(from), r.at(to)
	prev := begin
	var lag time.Duration
	for _, p := range r.Result.Shown {
		if p.Index < from || p.Index >= to {
			continue
		}
		s.Shown++
		lag += p.At.Sub(r.at(p.Index))
		if moving {
			s.MaxFreeze = max(s.MaxFreeze, p.At.Sub(prev))
		}
		prev = p.At
		s.PSNR += p.Score.PSNR
		s.SSIM += p.Score.SSIM
		s.MinPSNR = min(s.MinPSNR, p.Score.PSNR)
		s.MinSSIM = min(s.MinSSIM, p.Score.SSIM)
		s.WorstTile = min(s.WorstTile, p.Score.WorstTile)
		s.Last = p.Score
	}
	if s.Shown > 0 {
		s.PSNR /= float64(s.Shown)
		s.SSIM /= float64(s.Shown)
		s.Lag = lag / time.Duration(s.Shown)
	}
	s.FPS = float64(s.Shown) / s.Seconds
	for _, q := range r.Result.Requests {
		if !q.At.Before(begin) && q.At.Before(end) {
			s.Requests[q.Why]++
		}
	}
	return s
}

// Requests counts the run's keyframe requests by reason.
func (r *Run) Requests() map[string]int {
	out := map[string]int{}
	for _, q := range r.Result.Requests {
		out[q.Why]++
	}
	return out
}

// Table renders spans one per line.
func Table(spans []Span) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s %6s %6s %5s %5s %8s %5s %5s %7s %7s %6s %6s %6s %7s %6s %s\n",
		"span", "frames", "shown", "fps", "keys", "kbps", "qp", "psnr", "minpsnr", "ssim", "minss", "tile", "last", "freeze", "lag", "requests")
	for _, s := range spans {
		fmt.Fprintf(&b, "%-10s %6d %6d %5.1f %5d %8.0f %5.1f %5.1f %7.1f %7.4f %6.3f %6.3f %6.3f %7s %6s %s\n",
			s.Name, s.SentFrames, s.Shown, s.FPS, s.Keyframes, s.KbpsSent, s.MeanQP, s.PSNR, s.MinPSNR, s.SSIM,
			s.MinSSIM, s.WorstTile, s.Last.SSIM, s.MaxFreeze.Round(time.Millisecond), s.Lag.Round(time.Millisecond),
			formatCounts(s.Requests))
	}
	return b.String()
}

func formatCounts(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

// Summary is one line about the transport over a whole run.
func (r *Run) Summary() string {
	st := r.Result.Stats
	return fmt.Sprintf("packets %d (dup %d), NACKed %d, recovered %d, abandoned %d, frames %d (keyframes %d), decode errors %d, requests %s",
		st.Packets, st.Duplicates, st.NACKed, st.Recovered, st.Abandoned, st.Frames, st.Keyframes, r.Result.DecodeErrors,
		formatCounts(r.Requests()))
}
