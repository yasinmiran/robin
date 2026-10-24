package camera

import "github.com/yasinmiran/robin/internal/agent/h264"

// frame is one picture of the camera's fixed size, I420 with its planes back
// to back, as rawvideoparse reads them.
type frame []byte

func newFrame() frame { return make(frame, Width*Height*3/2) }

func (f frame) planes() (y, u, v []byte) {
	return f[:Width*Height], f[Width*Height : Width*Height*5/4], f[Width*Height*5/4:]
}

// black fills f with black, in the limited range of BT.601.
func (f frame) black() {
	y, u, v := f.planes()
	fillBytes(y, 16)
	fillBytes(u, 128)
	fillBytes(v, 128)
}

func fillBytes(b []byte, v byte) {
	for i := range b {
		b[i] = v
	}
}

// fitter scales pictures of one size into frames, keeping their shape: the
// picture fills the frame one way and is centred the other, on black. The
// browser changes its camera's resolution as the link and its CPU allow,
// while the host's programs need one size for as long as they hold the
// device. Nearest-neighbour sampling: the frame is shown, not analysed, and
// it costs about 2 ms a frame.
type fitter struct {
	srcW, srcH int
	// The fitted picture, in luma pixels of the frame, all even.
	x0, y0, w, h int
	// cols and rows map each column and row of the fitted picture, luma and
	// then chroma, to the source column and row it samples.
	cols, rows, ccols, crows []int
}

func newFitter(srcW, srcH int) *fitter {
	f := &fitter{srcW: srcW, srcH: srcH}
	if srcW*Height >= srcH*Width {
		f.w, f.h = Width, max(srcH*Width/srcW&^1, 2)
	} else {
		f.w, f.h = max(srcW*Height/srcH&^1, 2), Height
	}
	f.x0, f.y0 = (Width-f.w)/2&^1, (Height-f.h)/2&^1
	f.cols, f.rows = steps(srcW, f.w), steps(srcH, f.h)
	f.ccols, f.crows = steps((srcW+1)/2, f.w/2), steps((srcH+1)/2, f.h/2)
	return f
}

// steps maps each of n outputs to the source sample at its centre.
func steps(src, n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = min((2*i+1)*src/(2*n), src-1)
	}
	return s
}

// fit draws pic into dst, which it overwrites whole.
func (f *fitter) fit(dst frame, pic *h264.Picture) {
	y, u, v := dst.planes()
	if f.w < Width || f.h < Height {
		dst.black()
	}
	if f.w == f.srcW && f.h == f.srcH {
		for r := range f.h {
			copy(y[(f.y0+r)*Width+f.x0:][:f.w], pic.Y[r*pic.YStride:])
		}
		for r := range f.h / 2 {
			off := (f.y0/2+r)*(Width/2) + f.x0/2
			copy(u[off:][:f.w/2], pic.U[r*pic.CStride:])
			copy(v[off:][:f.w/2], pic.V[r*pic.CStride:])
		}
		return
	}
	for r, sr := range f.rows {
		out := y[(f.y0+r)*Width+f.x0:][:f.w]
		in := pic.Y[sr*pic.YStride:]
		for c, sc := range f.cols {
			out[c] = in[sc]
		}
	}
	for r, sr := range f.crows {
		off := (f.y0/2+r)*(Width/2) + f.x0/2
		ou, ov := u[off:][:f.w/2], v[off:][:f.w/2]
		iu, iv := pic.U[sr*pic.CStride:], pic.V[sr*pic.CStride:]
		for c, sc := range f.ccols {
			ou[c], ov[c] = iu[sc], iv[sc]
		}
	}
}
