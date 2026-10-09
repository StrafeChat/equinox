// Package weborigin knows which request origins belong to Strafe's own clients.
package weborigin

// desktop is what the Strafe desktop app's webview presents as its Origin: a custom
// scheme on macOS and Linux, a loopback-style host on Windows (plain or https, depending
// on the shell's settings). No web page can carry one of these - a browser derives Origin
// from the page's own URL - so admitting them lets the desktop app reach any instance
// without the operator listing anything, while CORS_ORIGINS and
// STARGATE_ALLOWED_ORIGINS keep doing their job against real web origins. A native app
// was never what those lists protect against anyway: it holds its token explicitly.
var desktop = map[string]bool{
	"tauri://localhost":       true,
	"http://tauri.localhost":  true,
	"https://tauri.localhost": true,
}

// IsDesktop reports whether origin is the desktop app.
func IsDesktop(origin string) bool { return desktop[origin] }
