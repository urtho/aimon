// Package poller refreshes every provider on a fixed interval, persists each
// snapshot to the store, and fans the latest results out to SSE subscribers.
package poller

import (
	"context"
	"log/slog"
	"sort"
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
	log       *slog.Logger

	mu      sync.RWMutex
	latest  map[string]model.Status
	hub     *hub
	trigger chan struct{}
}

// New builds a Poller. interval is the refresh cadence (e.g. 60s).
func New(st *store.Store, interval time.Duration, provs []providers.Provider, log *slog.Logger) *Poller {
	return &Poller{
		store:     st,
		interval:  interval,
		providers: provs,
		log:       log,
		latest:    make(map[string]model.Status),
		hub:       newHub(),
		trigger:   make(chan struct{}, 1),
	}
}

// Run blocks, polling immediately and then every interval until ctx is done.
func (p *Poller) Run(ctx context.Context) {
	// Seed the cache from the DB so the UI has data instantly after a restart.
	if seed, err := p.store.LatestStatuses(ctx); err == nil {
		p.mu.Lock()
		for _, s := range seed {
			p.latest[s.ID()] = s
		}
		p.mu.Unlock()
	}

	p.pollAll(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopping")
			return
		case <-ticker.C:
			p.pollAll(ctx)
		case <-p.trigger:
			p.pollAll(ctx)
		case <-prune.C:
			if n, err := p.store.Prune(ctx, time.Now().AddDate(0, 0, -30)); err == nil && n > 0 {
				p.log.Info("pruned old snapshots", "rows", n)
			}
		}
	}
}

// PollNow requests an out-of-band refresh (e.g. right after an OAuth connect).
func (p *Poller) PollNow() {
	select {
	case p.trigger <- struct{}{}:
	default: // a refresh is already queued
	}
}

// pollAll refreshes every provider concurrently and broadcasts the results.
func (p *Poller) pollAll(ctx context.Context) {
	var wg sync.WaitGroup
	results := make([]model.Status, len(p.providers))
	for i, prov := range p.providers {
		wg.Add(1)
		go func(i int, prov providers.Provider) {
			defer wg.Done()
			results[i] = p.pollOne(ctx, prov)
		}(i, prov)
	}
	wg.Wait()

	p.mu.Lock()
	for _, st := range results {
		p.latest[st.ID()] = st
		if err := p.store.WriteSnapshot(ctx, st); err != nil {
			p.log.Error("write snapshot", "id", st.ID(), "err", err)
		}
	}
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

// Latest returns the current status of every monitor, ordered stably.
func (p *Poller) Latest() []model.Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]model.Status, 0, len(p.latest))
	for _, s := range p.latest {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// Subscribe registers an SSE listener. The returned cancel func must be called
// to release the subscription.
func (p *Poller) Subscribe() (<-chan []model.Status, func()) {
	return p.hub.subscribe()
}
