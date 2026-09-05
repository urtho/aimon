// Package poller refreshes every provider on a fixed interval, persists each
// snapshot to the store, and fans the latest results out to SSE subscribers.
package poller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/algonode/aimon/internal/model"
	"github.com/algonode/aimon/internal/providers"
	"github.com/algonode/aimon/internal/store"
)

// Poller owns the refresh loop and the in-memory "latest status" cache.
type Poller struct {
	store     *store.Store
	interval  time.Duration
	providers []providers.Provider
	cadence   []time.Duration // per-provider period; 0 means interval
	lastPoll  []time.Time
	log       *slog.Logger

	mu      sync.RWMutex
	latest  map[string]model.Status
	hub     *hub
	trigger chan struct{}
}

// New builds a Poller. interval is the default refresh cadence (e.g. 60s).
// cadence is an optional per-provider period aligned with provs; a zero
// entry uses interval. Dashboard order follows provs, not alphabetical.
func New(st *store.Store, interval time.Duration, provs []providers.Provider, cadence []time.Duration, log *slog.Logger) *Poller {
	cad := make([]time.Duration, len(provs))
	for i := range provs {
		if i < len(cadence) && cadence[i] > 0 {
			cad[i] = cadence[i]
		} else {
			cad[i] = interval
		}
	}
	return &Poller{
		store:     st,
		interval:  interval,
		providers: provs,
		cadence:   cad,
		lastPoll:  make([]time.Time, len(provs)),
		log:       log,
		latest:    make(map[string]model.Status),
		hub:       newHub(),
		trigger:   make(chan struct{}, 1),
	}
}

// Run blocks, polling immediately and then every interval until ctx is done.
func (p *Poller) Run(ctx context.Context) {
	// Seed the cache from the DB so the UI has data instantly after a restart.
	// Only keep monitors that are still configured — a renamed label must not
	// leave a ghost card next to the replacement.
	want := p.configuredIDs()
	if seed, err := p.store.LatestStatuses(ctx); err == nil {
		p.mu.Lock()
		for _, s := range seed {
			if _, ok := want[s.ID()]; ok {
				p.latest[s.ID()] = s
			}
		}
		p.mu.Unlock()
	}

	p.pollAll(ctx, true)

	ticker := time.NewTicker(p.tickInterval())
	defer ticker.Stop()
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopping")
			return
		case <-ticker.C:
			p.pollAll(ctx, false)
		case <-p.trigger:
			p.pollAll(ctx, true)
		case <-prune.C:
			if n, err := p.store.Prune(ctx, time.Now().AddDate(0, 0, -30)); err == nil && n > 0 {
				p.log.Info("pruned old snapshots", "rows", n)
			}
		}
	}
}

func (p *Poller) tickInterval() time.Duration {
	tick := p.interval
	for _, d := range p.cadence {
		if d > 0 && d < tick {
			tick = d
		}
	}
	if tick <= 0 {
		return time.Minute
	}
	return tick
}

// PollNow requests an out-of-band refresh (e.g. right after an OAuth connect).
func (p *Poller) PollNow() {
	select {
	case p.trigger <- struct{}{}:
	default: // a refresh is already queued
	}
}

// pollAll refreshes due providers concurrently and broadcasts the results.
// force fetches every monitor regardless of its cadence (startup, PollNow).
func (p *Poller) pollAll(ctx context.Context, force bool) {
	now := time.Now()
	var wg sync.WaitGroup
	results := make([]model.Status, len(p.providers))
	fetched := make([]bool, len(p.providers))
	for i, prov := range p.providers {
		if !force && !p.lastPoll[i].IsZero() && now.Sub(p.lastPoll[i]) < p.cadence[i] {
			continue
		}
		fetched[i] = true
		wg.Add(1)
		go func(i int, prov providers.Provider) {
			defer wg.Done()
			results[i] = p.pollOne(ctx, prov)
		}(i, prov)
	}
	wg.Wait()

	p.mu.Lock()
	next := make(map[string]model.Status, len(p.providers))
	for k, v := range p.latest {
		next[k] = v
	}
	for i, ok := range fetched {
		if !ok {
			continue
		}
		st := results[i]
		next[st.ID()] = st
		p.lastPoll[i] = now
		if err := p.store.WriteSnapshot(ctx, st); err != nil {
			p.log.Error("write snapshot", "id", st.ID(), "err", err)
		}
	}
	p.latest = next
	p.mu.Unlock()

	p.hub.broadcast(p.Latest())
}

// pollOne fetches a single provider with a bounded timeout and normalizes the
// resulting Status (identity fields, error handling, timestamp).
func (p *Poller) pollOne(ctx context.Context, prov providers.Provider) model.Status {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	st, err := prov.Fetch(cctx)
	if err != nil {
		p.log.Warn("provider fetch failed", "provider", prov.ID(), "label", prov.Label(), "err", err)
		// Preserve any flags the provider set (e.g. NeedsAuth) on the error path.
		st.OK = false
		if st.Error == "" {
			st.Error = err.Error()
		}
	}
	if st.Provider == "" {
		st.Provider = prov.ID()
	}
	if st.Label == "" {
		st.Label = prov.Label()
	}
	if st.Kind == "" {
		st.Kind = prov.Kind()
	}
	st.UpdatedAt = time.Now().Unix()
	return st
}

func (p *Poller) configuredIDs() map[string]struct{} {
	want := make(map[string]struct{}, len(p.providers))
	for _, prov := range p.providers {
		want[prov.ID()+"/"+prov.Label()] = struct{}{}
	}
	return want
}

// Latest returns the current status of every monitor, in config order.
func (p *Poller) Latest() []model.Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]model.Status, 0, len(p.providers))
	for _, prov := range p.providers {
		if s, ok := p.latest[prov.ID()+"/"+prov.Label()]; ok {
			out = append(out, s)
		}
	}
	return out
}

// Subscribe registers an SSE listener. The returned cancel func must be called
// to release the subscription.
func (p *Poller) Subscribe() (<-chan []model.Status, func()) {
	return p.hub.subscribe()
}
