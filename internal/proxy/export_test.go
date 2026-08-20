package proxy

import "time"

// SetHealthClock replaces the read-health time source so tests can advance past
// the cool-off without sleeping through it. Test-only: export_test.go is
// compiled only under `go test`.
func SetHealthClock(p *Proxy, now func() time.Time) {
	p.health.mu.Lock()
	defer p.health.mu.Unlock()
	p.health.now = now
}

// ReadCoolOff exposes the cool-off window so tests can advance exactly past it
// rather than hard-coding a duration that would drift from the constant.
const ReadCoolOff = readCoolOff
