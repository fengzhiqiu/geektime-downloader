package job

import "sync/atomic"

// Stats counts terminal job errors by apperr code. In-memory, resets on restart.
// Zero value is ready to use. Unknown codes roll up into INTERNAL_ERROR.
type Stats struct {
	authExpired, rateLimited, timeout, internalErr atomic.Int64
}

// Inc increments the counter for the given apperr code.
func (s *Stats) Inc(code string) {
	switch code {
	case "AUTH_EXPIRED":
		s.authExpired.Add(1)
	case "RATE_LIMITED":
		s.rateLimited.Add(1)
	case "TIMEOUT":
		s.timeout.Add(1)
	default:
		s.internalErr.Add(1)
	}
}

// Snapshot returns a copy of current counts keyed by apperr code.
func (s *Stats) Snapshot() map[string]int64 {
	return map[string]int64{
		"AUTH_EXPIRED":   s.authExpired.Load(),
		"RATE_LIMITED":   s.rateLimited.Load(),
		"TIMEOUT":        s.timeout.Load(),
		"INTERNAL_ERROR": s.internalErr.Load(),
	}
}
