package gui

import (
	"errors"

	"github.com/yasinmiran/remotejump/internal/probe"
)

// pickAttach chooses the running desktop that --attach auto means.
func pickAttach(r *probe.Report) (string, error) {
	return "", errors.New("--attach auto is not available yet; name a display such as :0")
}
