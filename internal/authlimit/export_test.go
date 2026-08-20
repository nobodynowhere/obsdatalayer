package authlimit

import "time"

// SetClock replaces a limiter's time source so tests can advance time directly
// instead of sleeping. Test-only: export_test.go is compiled only under `go test`.
func SetClock(l *Limiter, now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}
