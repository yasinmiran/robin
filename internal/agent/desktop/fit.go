package desktop

// Fit scales w x h down to fit inside maxW x maxH, keeping the aspect ratio,
// and rounds to even dimensions of at least 2. A maxW or maxH of zero or
// less leaves the size unbounded, and an unknown size stays unknown (0, 0).
func Fit(w, h, maxW, maxH int) (int, int) {
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	if maxW > 0 && maxH > 0 && (w > maxW || h > maxH) {
		scale := min(float64(maxW)/float64(w), float64(maxH)/float64(h))
		w, h = int(float64(w)*scale), int(float64(h)*scale)
	}
	return max(w&^1, 2), max(h&^1, 2)
}
