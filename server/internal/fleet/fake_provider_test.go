package fleet

import (
	"context"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"sync"
)

// The only external boundary is a test-created provider: never Docker, CLI or credentials.
type fakeProvider struct {
	mu           sync.Mutex
	actions      []string
	availability func(context.Context) error
	diagnose     func(context.Context, model.Node, model.OperationRef) (model.Observation, error)
	inspect      func(context.Context, model.Node) (model.Observation, error)
}

func (p *fakeProvider) record(action string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, action)
}
func (p *fakeProvider) CheckAvailability(ctx context.Context) error {
	p.record("availability")
	if p.availability != nil {
		return p.availability(ctx)
	}
	return ctx.Err()
}
func (p *fakeProvider) Ensure(context.Context, model.Node, model.Bootstrap) (model.Observation, error) {
	p.record("ensure")
	return model.Observation{}, model.ErrUnavailable
}
func (p *fakeProvider) Inspect(ctx context.Context, n model.Node) (model.Observation, error) {
	p.record("inspect")
	if p.inspect != nil {
		return p.inspect(ctx, n)
	}
	return model.Observation{}, model.ErrUnavailable
}
func (p *fakeProvider) Apply(context.Context, model.Node, model.Action) (model.Observation, error) {
	p.record("apply")
	return model.Observation{}, model.ErrUnavailable
}
func (p *fakeProvider) Delete(context.Context, model.Node, model.OperationRef) error {
	p.record("delete")
	return model.ErrUnavailable
}
func (p *fakeProvider) Diagnose(ctx context.Context, n model.Node, ref model.OperationRef) (model.Observation, error) {
	p.record("diagnose")
	if p.diagnose != nil {
		return p.diagnose(ctx, n, ref)
	}
	return model.Observation{}, model.ErrUnavailable
}

var _ model.Provider = (*fakeProvider)(nil)
