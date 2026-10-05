package fleet

import "github.com/multica-ai/multica/server/internal/fleet/model"

// CanApplyOperation classifies eligibility only. SQL current-binding checks supply authority.
func CanApplyOperation(op model.Operation) bool {
	if op.Phase != "queued" && op.Phase != "prepared" && op.Phase != "applying" {
		return false
	}
	switch op.Action {
	case model.Create, model.Start:
		return true
	case model.Stop, model.Reboot, model.Delete:
		return op.Approved
	default:
		return false
	}
}
