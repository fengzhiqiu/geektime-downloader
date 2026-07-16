package job

import (
	"sync/atomic"

	"github.com/nicoxiang/geektime-downloader/internal/apperr"
)

// Stats counts terminal job errors by apperr code. In-memory, resets on restart.
// Zero value is ready to use. Unknown codes roll up into INTERNAL_ERROR.
type Stats struct {
	authExpired, rateLimited, timeout, internalErr atomic.Int64
}

// Inc increments the counter for the given apperr code.
func (s *Stats) Inc(code string) {
	switch code {
	case apperr.CodeAuthExpired:
		s.authExpired.Add(1)
	case apperr.CodeRateLimited:
		s.rateLimited.Add(1)
	case apperr.CodeTimeout:
		s.timeout.Add(1)
	default:
		s.internalErr.Add(1)
	}
}

// Snapshot returns a copy of current counts keyed by apperr code.
func (s *Stats) Snapshot() map[string]int64 {
	return map[string]int64{
		apperr.CodeAuthExpired:   s.authExpired.Load(),
		apperr.CodeRateLimited:   s.rateLimited.Load(),
		apperr.CodeTimeout:       s.timeout.Load(),
		apperr.CodeInternal:      s.internalErr.Load(),
	}
}
