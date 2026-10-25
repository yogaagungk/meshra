package relay

import (
	"context"
	"time"

	"meshra/internal/model"
)

// Probes are distinct from admitted inference turns. Editing or disabling a
// connection cancels discovery without rewriting an active turn's configuration.
func (r *Relay) beginConnectionProbe(ctx context.Context, connectionID string, timeout time.Duration) (context.Context, func(), error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, nil, model.Err(503, "relay is stopping")
	}
	if r.connectionProbeCount >= 32 {
		r.mu.Unlock()
		return nil, nil, model.Err(429, "connection probe capacity reached")
	}
	if r.connectionProbes == nil {
		r.connectionProbes = make(map[string]map[string]context.CancelFunc)
	}
	if r.connectionProbes[connectionID] == nil {
		r.connectionProbes[connectionID] = make(map[string]context.CancelFunc)
	}
	probeID := randomID()
	child, cancel := context.WithTimeout(ctx, timeout)
	r.connectionProbes[connectionID][probeID] = cancel
	r.connectionProbeCount++
	r.mu.Unlock()
	stop := context.AfterFunc(r.ctx, cancel)
	release := func() {
		cancel()
		stop()
		r.mu.Lock()
		defer r.mu.Unlock()
		probes := r.connectionProbes[connectionID]
		if _, exists := probes[probeID]; exists {
			delete(probes, probeID)
			r.connectionProbeCount--
		}
		if len(probes) == 0 {
			delete(r.connectionProbes, connectionID)
		}
	}
	return child, release, nil
}

// Caller holds relay ownership, after its connection mutation commits.
func (r *Relay) cancelConnectionProbesLocked(connectionID string) {
	for _, cancel := range r.connectionProbes[connectionID] {
		cancel()
	}
}
