package model

import (
	"fmt"
	"strings"
	"time"
)

func CanClaim(n Node, now time.Time) bool {
	return n.Desired == "running" && n.Status == "running" && n.Ready &&
		!n.Maintenance && !n.Revoked && !n.HealthAt.IsZero() &&
		!n.HealthAt.After(now) && now.Sub(n.HealthAt) <= 30*time.Second
}

func CanEnqueue(n Node) bool {
	return !n.Revoked && n.Desired != "terminated" && n.Desired != "terminating"
}

func ValidateCreate(req CreateRequest, cfg Config) error {
	if strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("%w: name required", ErrInvalidRequest)
	}
	if _, ok := cfg.Specs[req.Spec]; !ok {
		return fmt.Errorf("%w: declared spec required", ErrInvalidRequest)
	}
	return nil
}
