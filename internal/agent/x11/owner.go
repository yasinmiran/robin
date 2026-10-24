//go:build linux

package x11

import "os"

// trustedServerUIDs are the users whose X servers the agent talks to: this
// user, and root, which runs Xorg for display managers. A var so a test can
// make its own listener a stranger's.
var trustedServerUIDs = []uint32{uint32(os.Getuid()), 0}
