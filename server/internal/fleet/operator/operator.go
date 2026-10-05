package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/cloudruntime"
	"github.com/multica-ai/multica/server/internal/fleet/model"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/fleetguard"
	"github.com/multica-ai/multica/server/internal/util"
)

type Config struct {
	Namespace, FleetID, FleetURL, DatabaseURL, ConfigFile, ServiceKeyFile, ProfilesFile, OperationKey string
	Timeout                                                                                           time.Duration
}
type repository interface {
	CloseNamespace(context.Context, string, string) (store.NamespaceFence, error)
	BeginNamespaceDestroy(context.Context, store.NamespaceFence) (store.NamespaceFence, error)
	LookupNamespaceOperation(context.Context, model.Node, model.Action, string) (model.Operation, bool, error)
	OpenNamespace(context.Context, store.NamespaceFence) error
	GetNamespaceFence(context.Context) (store.NamespaceFence, error)
	ListNamespaceNodes(context.Context, pgtype.UUID, int32) ([]model.Node, error)
	UpsertProfiles(context.Context, map[pgtype.UUID]string, int64) error
	CheckNamespaceCompletion(context.Context, store.NamespaceFence, model.Node, model.OperationRef, string) error
	CheckNamespaceCompletions(context.Context, store.NamespaceFence, []store.NamespaceCompletion) error
}
type dependencies struct {
	repo    repository
	request func(context.Context, pgtype.UUID, pgtype.UUID, model.Action, string) (model.Operation, error)
}

// nodeActionKey preserves the same durable ref on retries, separating fence epochs.
func nodeActionKey(f store.NamespaceFence, n model.Node, a model.Action) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%s:%s", f.Namespace, f.OperationKey, f.Generation, util.UUIDToString(n.ID), a)))
	return "namespace-" + hex.EncodeToString(sum[:])
}
func run(ctx context.Context, action string, cfg Config, d dependencies) error {
	if d.repo == nil {
		return model.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	switch action {
	case "prepare":
		fence, err := d.repo.GetNamespaceFence(ctx)
		if err != nil {
			return err
		}
		if fence.Generation > 0 && (fence.Namespace != cfg.Namespace || fence.FleetID != cfg.FleetID) {
			return model.ErrConflict
		}
		owners, version, err := LoadProfiles(cfg.ProfilesFile)
		if err != nil {
			return err
		}
		return d.repo.UpsertProfiles(ctx, owners, version)
	case "status":
		f, err := d.repo.GetNamespaceFence(ctx)
		if err != nil {
			return err
		}
		if f.Generation > 0 && f.FleetID != cfg.FleetID {
			return model.ErrConflict
		}
		return nil
	case "resume":
		f, err := d.repo.GetNamespaceFence(ctx)
		if err != nil {
			return err
		}
		if f.Generation == 0 {
			return nil
		}
		if f.Namespace != cfg.Namespace || f.FleetID != cfg.FleetID || f.OperationKey != cfg.OperationKey || !f.Closed {
			return model.ErrConflict
		}
		return d.repo.OpenNamespace(ctx, f)
	case "quiesce", "destroy":
	default:
		return model.ErrInvalidRequest
	}
	f, err := d.repo.CloseNamespace(ctx, cfg.FleetID, cfg.OperationKey)
	if err != nil {
		return err
	}
	if !f.Closed || f.Namespace != cfg.Namespace || f.FleetID != cfg.FleetID || f.OperationKey != cfg.OperationKey || f.Generation < 1 {
		return model.ErrConflict
	}
	if f.Finalized {
		wanted := model.Stop
		if action == "destroy" {
			wanted = model.Delete
		}
		receipts, found, e := lookupCompletions(ctx, d.repo, f, wanted)
		if e != nil {
			return e
		}
		if found {
			return d.repo.CheckNamespaceCompletions(ctx, f, receipts)
		}
		if action != "destroy" {
			return model.ErrUnknownHealth
		}
		receipts, found, e = lookupCompletions(ctx, d.repo, f, model.Stop)
		if e != nil {
			return e
		}
		if !found {
			return model.ErrUnknownHealth
		}
		if e = d.repo.CheckNamespaceCompletions(ctx, f, receipts); e != nil {
			return e
		}
		old := f
		f, e = d.repo.BeginNamespaceDestroy(ctx, old)
		if e != nil {
			return e
		}
		if !f.Closed || f.Finalized || f.Namespace != old.Namespace || f.FleetID != old.FleetID || f.OperationKey != old.OperationKey || f.Generation != old.Generation+1 {
			return model.ErrConflict
		}
	}
	if d.request == nil {
		return model.ErrUnavailable
	}
	var after pgtype.UUID
	var completed []store.NamespaceCompletion
	for {
		nodes, err := d.repo.ListNamespaceNodes(ctx, after, 100)
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			break
		}
		for _, n := range nodes {
			if n.Namespace != cfg.Namespace || !n.ID.Valid || n.ID.Bytes == [16]byte{} || !n.OwnerID.Valid || n.OwnerID.Bytes == [16]byte{} || after.Valid && bytes.Compare(n.ID.Bytes[:], after.Bytes[:]) <= 0 {
				return model.ErrConflict
			}
			a := model.Stop
			if action == "destroy" {
				a = model.Delete
			}
			key := nodeActionKey(f, n, a)
			op, err := d.request(ctx, n.OwnerID, n.ID, a, key)
			if err != nil {
				return err
			}
			ref := model.OperationRef{Namespace: n.Namespace, NodeID: n.ID, OperationID: op.ID, Generation: op.Generation, Action: a}
			if !op.ID.Valid || op.OwnerID != n.OwnerID || op.NodeID != n.ID || op.Action != a || !op.Approved || op.Generation < 1 {
				return model.ErrUnknownHealth
			}
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				err = d.repo.CheckNamespaceCompletion(ctx, f, n, ref, key)
				if err == nil {
					break
				}
				if !errors.Is(err, model.ErrBusy) {
					return err
				}
				timer := time.NewTimer(100 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			completed = append(completed, store.NamespaceCompletion{Node: n, Ref: ref, Key: key})
			after = n.ID
		}
	}
	return d.repo.CheckNamespaceCompletions(ctx, f, completed)
}

// lookupCompletions performs only durable receipt reads on a finalized cycle.
// All SQL owners must match; a partial set is not completion or destroy authority.
func lookupCompletions(ctx context.Context, r repository, f store.NamespaceFence, a model.Action) ([]store.NamespaceCompletion, bool, error) {
	var after pgtype.UUID
	var receipts []store.NamespaceCompletion
	for {
		nodes, e := r.ListNamespaceNodes(ctx, after, 100)
		if e != nil {
			return nil, false, e
		}
		if len(nodes) == 0 {
			return receipts, true, nil
		}
		for _, n := range nodes {
			if n.Namespace != f.Namespace || !n.ID.Valid || n.ID.Bytes == [16]byte{} || !n.OwnerID.Valid || n.OwnerID.Bytes == [16]byte{} || after.Valid && bytes.Compare(n.ID.Bytes[:], after.Bytes[:]) <= 0 {
				return nil, false, model.ErrConflict
			}
			key := nodeActionKey(f, n, a)
			op, found, e := r.LookupNamespaceOperation(ctx, n, a, key)
			if e != nil {
				return nil, false, e
			}
			if !found {
				return nil, false, nil
			}
			ref := model.OperationRef{Namespace: f.Namespace, NodeID: n.ID, OperationID: op.ID, Generation: op.Generation, Action: a}
			receipts = append(receipts, store.NamespaceCompletion{Node: n, Ref: ref, Key: key})
			after = n.ID
		}
	}
}

// Validate performs no network, database or provider construction.
func Validate(action string, cfg Config) error { _, _, err := validateInputs(action, cfg); return err }
func validateInputs(action string, cfg Config) (model.Config, []byte, error) {
	fail := func() (model.Config, []byte, error) { return model.Config{}, nil, model.ErrInvalidRequest }
	switch action {
	case "prepare", "quiesce", "destroy", "resume", "status":
	default:
		return fail()
	}
	if !validInput(cfg.Namespace) || !validInput(cfg.FleetID) || !validInput(cfg.OperationKey) || cfg.Timeout <= 0 || cfg.Timeout > 10*time.Minute {
		return fail()
	}
	u, err := url.Parse(cfg.FleetURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fail()
	}
	dbURL, err := url.Parse(cfg.DatabaseURL)
	if err != nil || (dbURL.Scheme != "postgres" && dbURL.Scheme != "postgresql") || dbURL.Hostname() == "" || len(dbURL.Path) < 2 || dbURL.User == nil || dbURL.User.Username() == "" || dbURL.Fragment != "" {
		return fail()
	}
	if password, ok := dbURL.User.Password(); !ok || password == "" {
		return fail()
	}
	// The local operator never discovers HOME pgpass/cert or PG* credential inputs.
	if dbURL.Query().Get("sslmode") != "disable" {
		return fail()
	}
	if _, err = readPrivate(cfg.ConfigFile); err != nil {
		return fail()
	}
	public, err := model.LoadConfig(cfg.ConfigFile)
	if err != nil || public.Namespace != cfg.Namespace || public.FleetID != cfg.FleetID {
		return fail()
	}
	secret, err := readPrivate(cfg.ServiceKeyFile)
	if err != nil {
		return fail()
	}
	secret = bytes.TrimSpace(secret)
	if len(secret) == 0 || len(secret) > 4096 || strings.IndexFunc(string(secret), unicode.IsControl) >= 0 {
		return fail()
	}
	if action == "prepare" {
		if _, _, err = LoadProfiles(cfg.ProfilesFile); err != nil {
			return model.Config{}, nil, err
		}
	} else if _, err = readPrivate(cfg.ProfilesFile); err != nil {
		return fail()
	}
	return public, secret, nil
}
func validInput(v string) bool {
	return v != "" && len(v) <= 128 && strings.TrimSpace(v) == v && strings.IndexFunc(v, unicode.IsControl) < 0
}
func Run(ctx context.Context, action string, cfg Config) error {
	public, secret, err := validateInputs(action, cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return model.ErrInvalidRequest
	}
	poolCfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return model.ErrUnavailable
	}
	defer pool.Close()
	repo := store.New(pool, cfg.Namespace, store.WithProvisioningConfig(public), store.WithMaxNodes(public.MaxNodes))
	if err = repo.CheckSchema(ctx); err != nil {
		return err
	}
	client := cloudruntime.NewClient(cloudruntime.Config{BaseURL: cfg.FleetURL, ServiceSecret: secret, Timeout: 5 * time.Second})
	diagnose := func(ctx context.Context, n model.Node, ref model.OperationRef) (model.Observation, error) {
		return client.DiagnoseNode(ctx, util.UUIDToString(n.OwnerID), ref)
	}
	maintainer := &fleetguard.Maintainer{Repo: repo, Diagnose: diagnose}
	return run(ctx, action, cfg, dependencies{repo: repo, request: maintainer.Request})
}
