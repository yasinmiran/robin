package gui

import (
	"os/exec"
	"runtime"
)

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
