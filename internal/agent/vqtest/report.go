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
	// QP is the encoder's average quantiser for the frame, -1 or 0 when
	// it does not say.
	QP int
	// Target is the congestion controller's target when it was encoded,
	// in bits per second.
	Target int
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
	// KbpsTarget is the mean target the frames were encoded against.
	KbpsTarget float64
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
	Requests      map[PLIReason]int
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

// Window is the numbers of clip frames [from, to), counted as motion: a
// part of a segment, such as its first second.
func (r *Run) Window(name string, from, to int) Span { return r.span(name, from, to, true) }

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
		Requests: map[PLIReason]int{}, MinPSNR: MaxPSNR, MinSSIM: 1, WorstTile: 1}
	bytes, qps := 0, 0
	for _, f := range r.Sent {
		if f.Index < from || f.Index >= to {
			continue
		}
		s.SentFrames++
		bytes += f.Bytes
		s.KbpsTarget += float64(f.Target) / 1000
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
	if s.SentFrames > 0 {
		s.KbpsTarget /= float64(s.SentFrames)
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
func (r *Run) Requests() map[PLIReason]int {
	out := map[PLIReason]int{}
	for _, q := range r.Result.Requests {
		out[q.Why]++
	}
	return out
}

// Table renders spans one per line.
func Table(spans []Span) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s %6s %6s %5s %5s %8s %7s %5s %5s %7s %7s %6s %6s %6s %7s %6s %s\n",
		"span", "frames", "shown", "fps", "keys", "kbps", "target", "qp", "psnr", "minpsnr", "ssim", "minss", "tile", "last", "freeze", "lag", "requests")
	for _, s := range spans {
		fmt.Fprintf(&b, "%-10s %6d %6d %5.1f %5d %8.0f %7.0f %5.1f %5.1f %7.1f %7.4f %6.3f %6.3f %6.3f %7s %6s %s\n",
			s.Name, s.SentFrames, s.Shown, s.FPS, s.Keyframes, s.KbpsSent, s.KbpsTarget, s.MeanQP, s.PSNR, s.MinPSNR, s.SSIM,
			s.MinSSIM, s.WorstTile, s.Last.SSIM, s.MaxFreeze.Round(time.Millisecond), s.Lag.Round(time.Millisecond),
			formatCounts(s.Requests))
	}
	return b.String()
}

func formatCounts(m map[PLIReason]int) string {
	if len(m) == 0 {
		return "-"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

// Summary is one line about the transport over a whole run, and one about
// its sound when it had some.
func (r *Run) Summary() string {
	st := r.Result.Stats
	line := fmt.Sprintf("packets %d (dup %d), NACKed %d, recovered %d, abandoned %d, frames %d (keyframes %d), "+
		"decode errors %d, no picture %d, untagged %d, requests %s",
		st.Packets, st.Duplicates, st.NACKed, st.Recovered, st.Abandoned, st.Frames, st.Keyframes,
		r.Result.DecodeErrors, r.Result.NoPicture, r.Result.Untagged, formatCounts(r.Requests()))
	if d := r.SoundDelays(); d.N > 0 {
		line += fmt.Sprintf("\nsound %d packets, one way %v median, %v at the 99th percentile, %v at most",
			d.N, d.Median.Round(time.Millisecond), d.P99.Round(time.Millisecond), d.Max.Round(time.Millisecond))
	}
	return line
}

// Delays is how long sound packets took to arrive.
type Delays struct {
	N                int
	Median, P99, Max time.Duration
}

// SoundDelays sums up how long the run's sound took to arrive.
func (r *Run) SoundDelays() Delays {
	ds := make([]time.Duration, len(r.Result.Sound))
	for i, s := range r.Result.Sound {
		ds[i] = s.At.Sub(s.Sent)
	}
	if len(ds) == 0 {
		return Delays{}
	}
	slices.Sort(ds)
	return Delays{N: len(ds), Median: ds[len(ds)/2], P99: ds[len(ds)*99/100], Max: ds[len(ds)-1]}
}
