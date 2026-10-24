package probe

import "strings"

// VideoDevice is a v4l2loopback device: its path, such as /dev/video9, and
// the card label it was loaded with.
type VideoDevice struct {
	Path, Label string
}

// CameraDevice picks the v4l2loopback device for the browser's camera: one
// whose label names Robin, so a device set aside for another program such
// as OBS is left to it, else the first. It is "" when there is none.
func (r *Report) CameraDevice() string {
	for _, d := range r.V4L2Loopback {
		if strings.Contains(strings.ToLower(d.Label), "robin") {
			return d.Path
		}
	}
	if len(r.V4L2Loopback) > 0 {
		return r.V4L2Loopback[0].Path
	}
	return ""
}
