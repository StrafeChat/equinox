// Package safego runs background goroutines that cannot take the process down with them.
//
// Fiber's recover middleware only covers the goroutine handling the request. Anything
// launched with a bare `go func()` - a federation delivery, a fan-out over rooms - is on
// its own: one nil dereference on an unexpected response, and every connected user's API
// disappears. That is a much worse outcome than one background task failing, so every
// goroutine that outlives or runs beside a request goes through here instead.
package safego

import (
	"runtime/debug"

	"github.com/StrafeChat/equinox/internal/logger"
)

// Go runs fn on a new goroutine, logging a panic under `tag` instead of crashing.
func Go(tag string, fn func()) {
	go Run(tag, fn)
}

// Run calls fn on the current goroutine with the same panic protection as Go. For use
// inside an existing goroutine (a WaitGroup worker, for instance) that must not escape.
func Run(tag string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error(tag, "panic in background task: %v\n%s", r, debug.Stack())
		}
	}()
	fn()
}
