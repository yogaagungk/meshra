package relay

import (
	"context"

	"meshra/internal/model"
)

// A live validated console session owns this context. The private registry has
// no authority-bearing HTTP fields and invokes no validator while locked.
func (r *Relay) beginConsoleJob(ctx context.Context, owner string) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if r.ctx.Err() != nil {
		return nil, nil, model.Err(503, "relay is stopping")
	}
	principal := model.Principal{Admin: true, ConsoleSessionID: owner}
	if err := r.requireConsole(principal); err != nil {
		return nil, nil, err
	}
	child, cancel := context.WithCancel(ctx)
	id := randomID()
	r.sessionMu.Lock()
	if r.revokedConsoleSessions[owner] {
		r.sessionMu.Unlock()
		cancel()
		return nil, nil, model.Err(403, "console session was revoked")
	}
	if r.consoleJobCount >= 32 {
		r.sessionMu.Unlock()
		cancel()
		return nil, nil, model.Err(429, "console job capacity reached")
	}
	if r.consoleJobs == nil {
		r.consoleJobs = make(map[string]map[string]context.CancelFunc)
	}
	if r.consoleJobs[owner] == nil {
		r.consoleJobs[owner] = make(map[string]context.CancelFunc)
	}
	r.consoleJobs[owner][id] = cancel
	r.consoleJobCount++
	r.sessionMu.Unlock()
	stop := context.AfterFunc(r.ctx, cancel)
	release := func() {
		cancel()
		stop()
		r.sessionMu.Lock()
		defer r.sessionMu.Unlock()
		jobs := r.consoleJobs[owner]
		if _, exists := jobs[id]; exists {
			delete(jobs, id)
			r.consoleJobCount--
		}
		if len(jobs) == 0 {
			delete(r.consoleJobs, owner)
		}
	}
	if err := r.requireConsole(principal); err != nil {
		release()
		return nil, nil, err
	}
	return child, release, nil
}
