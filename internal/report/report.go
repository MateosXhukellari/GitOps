package report

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrPolicy = errors.New("policy") // policy check rejected a revision
	ErrPrune  = errors.New("prune")  // a failure while pruning
)

type Outcome struct {
	mu      sync.Mutex
	applied bool
	pending bool
	pruned  int
}

type ctxKey struct{}

func With(ctx context.Context) (context.Context, *Outcome) {
	o := &Outcome{}
	return context.WithValue(ctx, ctxKey{}, o), o
}

func from(ctx context.Context) *Outcome {
	o, _ := ctx.Value(ctxKey{}).(*Outcome)
	return o
}

func MarkApplied(ctx context.Context) {
	if o := from(ctx); o != nil {
		o.mu.Lock()
		o.applied = true
		o.mu.Unlock()
	}
}

func MarkPending(ctx context.Context) {
	if o := from(ctx); o != nil {
		o.mu.Lock()
		o.pending = true
		o.mu.Unlock()
	}
}

func MarkPruned(ctx context.Context, n int) {
	if o := from(ctx); o != nil {
		o.mu.Lock()
		o.pruned += n
		o.mu.Unlock()
	}
}

func (o *Outcome) Applied() bool { o.mu.Lock(); defer o.mu.Unlock(); return o.applied }
func (o *Outcome) Pending() bool { o.mu.Lock(); defer o.mu.Unlock(); return o.pending }
func (o *Outcome) Pruned() int   { o.mu.Lock(); defer o.mu.Unlock(); return o.pruned }
