// Command rj-consent answers the portal's consent dialog on a Wayland test
// host, standing in for the person at its screen. Only the e2e host image
// carries it; no product binary links wltest.
//
//	rj-consent            share with remote interaction allowed
//	rj-consent view-only  share without it
package main

import (
	"fmt"
	"os"

	"github.com/yasinmiran/remotejump/internal/agent/wayland/wltest"
)

func main() {
	allowInput := len(os.Args) < 2 || os.Args[1] != "view-only"
	if err := wltest.AnswerDialog(allowInput); err != nil {
		fmt.Fprintln(os.Stderr, "rj-consent:", err)
		os.Exit(1)
	}
}
