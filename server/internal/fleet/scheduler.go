package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
)

// One slot retains the actual private winner in its closure, never a reconstructed claim.
// Slot and lifecycle fields are protected by the serial scheduler mutex; completion only closes done.
type initializationSlot struct {
	ref    model.OperationRef
	done   chan struct{}
	cancel context.CancelFunc
}

func (r *Reconciler) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}
func (r *Reconciler) reapInitialization() {
	if r.initialization == nil {
		return
	}
	select {
	case <-r.initialization.done:
		r.initialization.cancel()
		r.initialization = nil
	default:
	}
}
func (r *Reconciler) startInitialization(scheduler, service context.Context, s store.RecoverySnapshot) error {
	if r.initialization != nil {
		return nil
	} // No durable checkpoint for a waiting node; revisit by cursor.
	if r.claimBootstrap == nil {
		return model.ErrUnavailable
	}
	claim, e := r.claimBootstrap(scheduler, s)
	if e != nil {
		if errors.Is(e, model.ErrConflict) {
			return nil
		}
		return r.recordError(scheduler, s, e)
	}
	// Inherit the original Worker/service cancellation, not the short scheduling deadline.
	// claim.Context in initialize further caps this at the winner's original SQL-clock budget.
	ctx, cancel := context.WithCancel(service)
	slot := &initializationSlot{ref: s.Ref(), done: make(chan struct{}), cancel: cancel}
	r.initialization = slot
	go func() { defer close(slot.done); defer cancel(); _ = r.initialize(ctx, s, claim) }()
	return nil
}

// Run closes admission, cancels, and joins before process composition closes SQL/Engine resources.
func (r *Reconciler) closeInitialization() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopping = true
	if r.initialization != nil {
		r.initialization.cancel()
		<-r.initialization.done
		r.initialization = nil
	}
	r.running = false
}
