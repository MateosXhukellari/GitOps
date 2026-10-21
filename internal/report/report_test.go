package report

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestMarksAreRecorded(t *testing.T) {
	ctx, o := With(context.Background())
	if o.Applied() || o.Pending() || o.Pruned() != 0 {
		t.Fatalf("fresh outcome not empty: applied=%v pending=%v pruned=%d", o.Applied(), o.Pending(), o.Pruned())
	}
	MarkApplied(ctx)
	MarkPending(ctx)
	MarkPruned(ctx, 2)
	MarkPruned(ctx, 1)
	if !o.Applied() || !o.Pending() || o.Pruned() != 3 {
		t.Fatalf("got applied=%v pending=%v pruned=%d, want true true 3", o.Applied(), o.Pending(), o.Pruned())
	}
}

func TestMarksWithoutOutcomeAreNoOps(t *testing.T) {
	ctx := context.Background()
	// Must not panic.
	MarkApplied(ctx)
	MarkPending(ctx)
	MarkPruned(ctx, 1)
}

func TestWrappingKeepsMessage(t *testing.T) {
	err := fmt.Errorf("%w: 1 violation(s): bad", ErrPolicy)
	if !errors.Is(err, ErrPolicy) || errors.Is(err, ErrPrune) {
		t.Fatalf("errors.Is mismatch for %v", err)
	}
	if err.Error() != "policy: 1 violation(s): bad" {
		t.Fatalf("message = %q", err.Error())
	}
}
