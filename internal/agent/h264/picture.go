package h264

// Picture is a decoded frame in I420, its planes rows of YStride and
// CStride bytes. Width and Height are the picture's own size, which the
// strides may exceed.
type Picture struct {
	Width, Height    int
	YStride, CStride int
	Y, U, V          []byte
}
