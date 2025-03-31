package middleware

import (
	"fmt"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
)

type RateLimiter struct {
	store    map[string][]time.Time
	mutex    sync.Mutex
	window   time.Duration
	max      int
	cleanup  *time.Ticker
	endpoint string
}

// NewRateLimiter creates a new rate limiter for a specific endpoint
func NewRateLimiter(window time.Duration, max int, endpoint string) *RateLimiter {
	rl := &RateLimiter{
		store:    make(map[string][]time.Time),
		window:   window,
		max:      max,
		endpoint: endpoint,
	}

	// Start cleanup routine
	rl.cleanupRoutine()

	return rl
}

// cleanupRoutine periodically removes old entries from the store
func (rl *RateLimiter) cleanupRoutine() {
	rl.cleanup = time.NewTicker(rl.window)
	go func() {
		for range rl.cleanup.C {
			rl.mutex.Lock()
			now := time.Now()
			for ip, times := range rl.store {
				var validTimes []time.Time
				for _, t := range times {
					if now.Sub(t) < rl.window {
						validTimes = append(validTimes, t)
					}
				}
				if len(validTimes) == 0 {
					delete(rl.store, ip)
				} else {
					rl.store[ip] = validTimes
				}
			}
			rl.mutex.Unlock()
		}
	}()
}

// Stop stops the cleanup routine
func (rl *RateLimiter) Stop() {
	if rl.cleanup != nil {
		rl.cleanup.Stop()
	}
}

// Middleware returns a Fiber middleware function
func (rl *RateLimiter) Middleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		// Skip rate limiting if not the target endpoint
		if c.Path() != rl.endpoint {
			return c.Next()
		}

		ip := GetRealIP(c)
		rl.mutex.Lock()
		defer rl.mutex.Unlock()

		now := time.Now()
		times := rl.store[ip]

		// Remove timestamps outside the window
		var validTimes []time.Time
		for _, t := range times {
			if now.Sub(t) < rl.window {
				validTimes = append(validTimes, t)
			}
		}

		// Check if the number of requests exceeds the limit
		if len(validTimes) >= rl.max {
			retryAfter := rl.window - now.Sub(validTimes[0])
			c.Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())))
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error":       "Too many requests",
				"retry_after": int(retryAfter.Seconds()),
			})
		}

		// Add current timestamp
		validTimes = append(validTimes, now)
		rl.store[ip] = validTimes

		return c.Next()
	}
}

// PasswordResetRateLimiter creates a rate limiter specifically for password reset endpoints
func PasswordResetRateLimiter() fiber.Handler {
	// Allow 5 requests per 15 minutes
	rl := NewRateLimiter(15*time.Minute, 5, "/auth/password-reset")
	return rl.Middleware()
}

// PasswordResetVerifyRateLimiter creates a rate limiter for password reset verification
func PasswordResetVerifyRateLimiter() fiber.Handler {
	// Allow 10 attempts per 15 minutes
	rl := NewRateLimiter(15*time.Minute, 10, "/auth/password-reset/verify")
	return rl.Middleware()
}

// PasswordResetCompleteRateLimiter creates a rate limiter for password reset completion
func PasswordResetCompleteRateLimiter() fiber.Handler {
	// Allow 3 attempts per 15 minutes
	rl := NewRateLimiter(15*time.Minute, 3, "/auth/password-reset/complete")
	return rl.Middleware()
}
