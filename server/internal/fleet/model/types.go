// Package model defines provider-independent local fleet contracts.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type Action string

const (
	Create Action = "create"
	Start  Action = "start"
	Stop   Action = "stop"
	Reboot Action = "reboot"
	Delete Action = "delete"
)

var (
	ErrBusy           = errors.New("fleet node busy")
	ErrConflict       = errors.New("fleet conflict")
	ErrForbidden      = errors.New("fleet forbidden")
	ErrUnavailable    = errors.New("fleet unavailable")
	ErrUnknownHealth  = errors.New("fleet health unknown")
	ErrProfileMissing = errors.New("fleet profile missing")
	ErrInvalidRequest = errors.New("invalid fleet request")
)

type Node struct {
	// Observation is an internal durable diagnostic, never maintenance approval.
	Observation                                                     Observation
	Resources                                                       Spec
	CreatedAt, UpdatedAt                                            time.Time
	ID, OwnerID                                                     pgtype.UUID
	Namespace, ContainerID, DaemonID, Name, Spec, Image, ProfileRef string
	StartEpoch, DataVolume, SecretsVolume, ErrorCode, ErrorMessage  string
	Desired, Status                                                 string
	Generation                                                      int64
	Ready                                                           bool
	HealthAt                                                        time.Time
	ActiveRuns, PendingReports, FailedReports                       int
	Maintenance                                                     bool
	Revoked                                                         bool
}

type Operation struct {
	ActionClaimedAt                                  time.Time
	ActionStartEpoch                                 string
	BootstrapClaimedAt, NextAttemptAt                time.Time
	BootstrapMinted, NonRetryable                    bool
	ErrorCode                                        string
	CreatedAt, UpdatedAt                             time.Time
	ID, NodeID, OwnerID                              pgtype.UUID
	Action                                           Action
	Phase, IdempotencyKey, RequestHash, PriorDesired string
	Generation                                       int64
	Approved                                         bool
	Attempts                                         int
}

type OperationRef struct {
	Namespace           string
	NodeID, OperationID pgtype.UUID
	Generation          int64
	Action              Action
}

type CreateRequest struct{ Name, Spec, IdempotencyKey string }

// Bootstrap contains private provider inputs, not public configuration or log data.
type Bootstrap struct {
	NodeToken string `json:"node_token"`
	APIKey    string `json:"api_key"`
	BaseURL   string `json:"base_url,omitempty"`
	Model     string `json:"model,omitempty"`
	ServerURL string `json:"server_url"`
	DaemonID  string `json:"daemon_id"`
	// EnrollmentToken is the Aurora managed-enrollment secret. It is a private
	// input delivered once into the node secrets volume and never persisted.
	EnrollmentToken string `json:"enrollment_token,omitempty"`
}

// ReportStatsKnown is false until explicitly observed. Offline never implies Ready.
type Observation struct {
	ContainerID, Status, DaemonID, StartEpoch               string
	Ready                                                   bool
	Agents                                                  []string
	RuntimeCount, ActiveRuns, PendingReports, FailedReports int
	ReportStatsKnown                                        bool
	Offline                                                 bool
	DataVolume, LayoutVersion                               string
	ObservedAt                                              time.Time
}

type Provider interface {
	// CheckAvailability is read-only and must not create resources or inspect a fabricated node.
	CheckAvailability(context.Context) error
	Ensure(context.Context, Node, Bootstrap) (Observation, error)
	Inspect(context.Context, Node) (Observation, error)
	Apply(context.Context, Node, Action) (Observation, error)
	Delete(context.Context, Node, OperationRef) error
	Diagnose(context.Context, Node, OperationRef) (Observation, error)
}
