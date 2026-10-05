package fleet

import (
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"testing"
)

// An unapproved destructive action or an inactive phase must never become physically eligible.
func TestOperationRequiresApproval(t *testing.T) {
	for _, tc := range []struct {
		action         model.Action
		phase          string
		approved, want bool
	}{
		{model.Delete, "prepared", false, false}, {model.Delete, "prepared", true, true},
		{model.Stop, "queued", false, false}, {model.Stop, "applying", true, true},
		{model.Reboot, "queued", false, false}, {model.Reboot, "queued", true, true},
		{model.Create, "queued", false, true}, {model.Start, "prepared", false, true},
		{model.Delete, "preparing", true, false}, {model.Delete, "completed", true, false},
		{model.Start, "failed", true, false}, {model.Action("invalid"), "queued", true, false},
	} {
		op := model.Operation{Action: tc.action, Phase: tc.phase, Approved: tc.approved}
		if got := CanApplyOperation(op); got != tc.want {
			t.Errorf("action=%s phase=%s approved=%v: got %v want %v", tc.action, tc.phase, tc.approved, got, tc.want)
		}
	}
}
