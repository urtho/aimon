package poller

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/algonode/aimon/internal/model"
	"github.com/algonode/aimon/internal/providers"
	"github.com/algonode/aimon/internal/store"
)

type stub struct {
	id, label string
	n         atomic.Int32
}

func (s *stub) ID() string    { return s.id }
func (s *stub) Label() string { return s.label }
func (s *stub) Kind() string  { return model.KindCredit }
func (s *stub) Fetch(context.Context) (model.Status, error) {
	s.n.Add(1)
	v := 1.0
	return model.Status{Provider: s.id, Label: s.label, Kind: s.Kind(), OK: true, Balance: &v}, nil
}

func testPoller(t *testing.T, interval time.Duration, stubs []*stub, cadence []time.Duration) *Poller {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "aimon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	provs := make([]providers.Provider, len(stubs))
	for i, s := range stubs {
		provs[i] = s
	}
	return New(db, interval, provs, cadence, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestLatestPreservesConfigOrder(t *testing.T) {
	a := &stub{id: "xai", label: "Grok"}
	b := &stub{id: "deepseek", label: "DeepSeek"}
	c := &stub{id: "anthropic", label: "Anthropic API"}
	p := testPoller(t, time.Hour, []*stub{a, b, c}, nil)
	p.pollAll(context.Background(), true)
	got := p.Latest()
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	want := []string{"xai/Grok", "deepseek/DeepSeek", "anthropic/Anthropic API"}
	for i, id := range want {
		if got[i].ID() != id {
			t.Fatalf("order[%d]=%s, want %s (must not sort alphabetically)", i, got[i].ID(), id)
		}
	}
}

func TestPerMonitorCadenceSkipsUntilDue(t *testing.T) {
	fast := &stub{id: "deepseek", label: "DeepSeek"}
	slow := &stub{id: "anthropic", label: "Anthropic API"}
	p := testPoller(t, time.Minute, []*stub{fast, slow}, []time.Duration{time.Minute, 30 * time.Minute})
	ctx := context.Background()
	p.pollAll(ctx, true)
	if fast.n.Load() != 1 || slow.n.Load() != 1 {
		t.Fatalf("initial fetch fast=%d slow=%d", fast.n.Load(), slow.n.Load())
	}
	p.pollAll(ctx, false)
	if fast.n.Load() != 1 || slow.n.Load() != 1 {
		t.Fatalf("immediate tick must skip both: fast=%d slow=%d", fast.n.Load(), slow.n.Load())
	}
	p.lastPoll[0] = time.Now().Add(-2 * time.Minute)
	p.pollAll(ctx, false)
	if fast.n.Load() != 2 || slow.n.Load() != 1 {
		t.Fatalf("only due monitor should refetch: fast=%d slow=%d", fast.n.Load(), slow.n.Load())
	}
	p.pollAll(ctx, true)
	if slow.n.Load() != 2 {
		t.Fatalf("force poll should fetch the slow monitor: %d", slow.n.Load())
	}
}
