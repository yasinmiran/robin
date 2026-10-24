package gui

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/yasinmiran/robin/internal/web"
)

// listenPage binds the page server, which says on stderr when its launch
// link is opened a second time: the browser that did gets Not Found, and if
// that was the user's, someone else opened the page first.
func listenPage(o Options) (*web.Server, error) {
	return web.Listen(web.Options{Addr: o.Listen, LaunchReused: func() {
		fmt.Fprintln(o.Stderr, "robin: the page's launch link was opened again; if your browser shows Not Found, someone else on this machine opened the page first: end the session with Ctrl+C")
	}})
}

// showPage brings the user to the page: the browser opener gets the launch
// link, since its command line is there for every user of this machine to
// read, and the URL with the token goes only to stdout or stderr.
func showPage(o Options, srv *web.Server) {
	if o.NoBrowser {
		fmt.Fprintln(o.Stdout, srv.URL())
	} else if err := openBrowser(srv.LaunchURL()); err != nil {
		fmt.Fprintf(o.Stderr, "open this URL in your browser: %s\n", srv.URL())
	}
}

// openBrowser hands url to the program that opens it in the user's browser.
func openBrowser(url string) error {
	name, args := browserCommand(runtime.GOOS, exec.LookPath, url)
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the opener when it exits; it would otherwise stay a zombie for the
	// whole session.
	go cmd.Wait()
	return nil
}

// browserCommand returns the program that opens url on goos, and its
// arguments; lookPath finds programs. Termux, the Linux userland on Android,
// has no xdg-open; its termux-open-url hands the URL to Android's browser.
func browserCommand(goos string, lookPath func(string) (string, error), url string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{url}
	case "windows":
		// rundll32 passes the URL to the default browser untouched, where
		// cmd /c start would parse it as a command line first.
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	}
	if _, err := lookPath("termux-open-url"); err == nil {
		return "termux-open-url", []string{url}
	}
	return "xdg-open", []string{url}
}
