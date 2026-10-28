package manager

import "time"

// backoff returns the delay before retry n (counting from 0):
// min(1s << n, limit). Large n saturates at max instead of overflowing.
func backoff(n int, limit time.Duration) time.Duration {
	if n < 0 {
		n = 0
	}
	if n > 30 { // 1s<<34 overflows time.Duration; 1s<<30 is already ~34 years
		return limit
	}
	if d := time.Second << n; d < limit {
		return d
	}
	return limit
}
