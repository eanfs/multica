package model

import (
	"errors"
	"testing"
	"time"
)

func TestCanClaim(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		change func(*Node)
		want   bool
	}{
		{"ready", func(n *Node) {}, true},
		{"maintenance", func(n *Node) { n.Maintenance = true }, false},
		{"revoked", func(n *Node) { n.Revoked = true }, false},
		{"stale", func(n *Node) { n.HealthAt = now.Add(-31 * time.Second) }, false},
		{"boundary", func(n *Node) { n.HealthAt = now.Add(-30 * time.Second) }, true},
		{"just expired", func(n *Node) { n.HealthAt = now.Add(-30*time.Second - time.Nanosecond) }, false},
		{"future", func(n *Node) { n.HealthAt = now.Add(time.Nanosecond) }, false},
		{"missing health", func(n *Node) { n.HealthAt = time.Time{} }, false},
		{"starting", func(n *Node) { n.Status = "starting" }, false},
		{"stopped desired", func(n *Node) { n.Desired = "stopped" }, false},
		{"missing readiness", func(n *Node) { n.Ready = false }, false},
		{"pending reports", func(n *Node) { n.PendingReports = 1 }, true},
		{"failed reports", func(n *Node) { n.FailedReports = 1 }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := Node{Desired: "running", Status: "running", Ready: true, HealthAt: now}
			tc.change(&n)
			if got := CanClaim(n, now); got != tc.want {
				t.Fatalf("CanClaim = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCanEnqueue(t *testing.T) {
	for _, tc := range []struct {
		desired       string
		revoked, want bool
	}{
		{"running", false, true}, {"stopped", false, true}, {"starting", false, true},
		{"terminating", false, false}, {"terminated", false, false}, {"running", true, false},
	} {
		t.Run(tc.desired+map[bool]string{true: " revoked", false: ""}[tc.revoked], func(t *testing.T) {
			if got := CanEnqueue(Node{Desired: tc.desired, Revoked: tc.revoked}); got != tc.want {
				t.Fatalf("CanEnqueue = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateCreate(t *testing.T) {
	cfg := Config{Specs: map[string]Spec{"small": {CPUs: 2, MemoryBytes: 4294967296, Pids: 256, MaxRuns: 1}}}
	for _, tc := range []struct {
		name  string
		req   CreateRequest
		valid bool
	}{
		{"declared spec", CreateRequest{Name: "My node", Spec: "small", IdempotencyKey: "key"}, true},
		{"empty name", CreateRequest{Spec: "small"}, false},
		{"blank name", CreateRequest{Name: " \t\n", Spec: "small"}, false},
		{"empty spec", CreateRequest{Name: "node"}, false},
		{"undeclared spec", CreateRequest{Name: "node", Spec: "override"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCreate(tc.req, cfg)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
