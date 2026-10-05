package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	CheckNamespaceCompletions(context.Context, store.NamespaceFence, model.Action, []store.NamespaceCompletion) error
	GetNamespaceCompletions(context.Context, store.NamespaceFence, model.Action) ([]store.NamespaceCompletion, bool, error)
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
			return d.repo.CheckNamespaceCompletions(ctx, f, wanted, receipts)
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
		if e = d.repo.CheckNamespaceCompletions(ctx, f, model.Stop, receipts); e != nil {
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
	a := model.Stop
	if action == "destroy" {
		a = model.Delete
	}
	return d.repo.CheckNamespaceCompletions(ctx, f, a, completed)
}

// lookupCompletions performs only durable receipt reads on a finalized cycle.
// All SQL owners must match; a partial set is not completion or destroy authority.
func lookupCompletions(ctx context.Context, r repository, f store.NamespaceFence, a model.Action) ([]store.NamespaceCompletion, bool, error) {
	return r.GetNamespaceCompletions(ctx, f, a)
}

// Validate performs no network, database or provider construction.
func Validate(action string, cfg Config) error { _, _, err := validateInputs(action, cfg); return err }
func validateInputs(action string, cfg Config) (model.Config, []byte, error) {
	return validateInputsWithReader(action, cfg, readPrivate)
}
func validateInputsWithReader(action string, cfg Config, read func(string) ([]byte, error)) (model.Config, []byte, error) {
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
	if _, err := localDatabaseURI(cfg.DatabaseURL); err != nil {
		return fail()
	}
	raw, err := read(cfg.ConfigFile)
	if err != nil {
		return fail()
	}
	public, err := model.DecodeConfig(raw)
	if err != nil || public.Namespace != cfg.Namespace || public.FleetID != cfg.FleetID {
		return fail()
	}
	secret, err := read(cfg.ServiceKeyFile)
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
	} else if _, err = read(cfg.ProfilesFile); err != nil {
		return fail()
	}
	return public, secret, nil
}

// localDatabaseURI bounds the interpretation before pgx can discover files.
func localDatabaseURI(raw string) (string, error) {
	// Callers must omit HOME in the child environment, not mutate the parent.
	// On macOS/Linux this makes os.UserHomeDir fail before pgx home defaults.
	if os.Getenv("HOME") != "" {
		return "", model.ErrInvalidRequest
	}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") && value != "" {
			return "", model.ErrInvalidRequest
		}
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || strings.ContainsAny(u.Hostname(), ",/\\") || u.User == nil || u.User.Username() == "" || len(u.Path) < 2 || u.Fragment != "" || u.Opaque != "" {
		return "", model.ErrInvalidRequest
	}
	if pw, ok := u.User.Password(); !ok || pw == "" || strings.TrimSpace(pw) != pw || strings.IndexFunc(pw, unicode.IsControl) >= 0 {
		return "", model.ErrInvalidRequest
	}
	for _, value := range []string{u.Hostname(), u.User.Username(), strings.TrimPrefix(u.Path, "/")} {
		if value == "" || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", model.ErrInvalidRequest
		}
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return "", model.ErrInvalidRequest
		}
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", model.ErrInvalidRequest
	}
	for key, values := range q {
		if len(values) != 1 {
			return "", model.ErrInvalidRequest
		}
		switch key {
		case "sslmode":
			if values[0] != "disable" {
				return "", model.ErrInvalidRequest
			}
		case "application_name":
			if values[0] == "" || len(values[0]) > 128 || !utf8.ValidString(values[0]) || strings.IndexFunc(values[0], unicode.IsControl) >= 0 {
				return "", model.ErrInvalidRequest
			}
		case "connect_timeout":
			n, e := strconv.Atoi(values[0])
			if e != nil || n < 1 || n > 600 {
				return "", model.ErrInvalidRequest
			}
		default:
			return "", model.ErrInvalidRequest
		}
	}
	if q.Get("sslmode") != "disable" {
		return "", model.ErrInvalidRequest
	}
	// pgx/libpq URI decoding treats plus literally, unlike net/url query decoding.
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return u.String(), nil
}
func localPoolConfig(raw string) (*pgxpool.Config, error) {
	return localPoolConfigWithParser(raw, pgxpool.ParseConfig)
}
func localPoolConfigWithParser(raw string, parse func(string) (*pgxpool.Config, error)) (*pgxpool.Config, error) {
	canonical, err := localDatabaseURI(raw)
	if err != nil {
		return nil, err
	}
	cfg, err := parse(canonical)
	if err != nil {
		return nil, model.ErrInvalidRequest
	}
	u, _ := url.Parse(canonical)
	pw, _ := u.User.Password()
	if cfg.ConnConfig.Host != u.Hostname() || cfg.ConnConfig.Database != strings.TrimPrefix(u.Path, "/") || cfg.ConnConfig.User != u.User.Username() || cfg.ConnConfig.Password != pw || cfg.ConnConfig.TLSConfig != nil || len(cfg.ConnConfig.Fallbacks) != 0 {
		return nil, model.ErrInvalidRequest
	}
	expectedPort := uint16(5432)
	if u.Port() != "" {
		n, _ := strconv.Atoi(u.Port())
		expectedPort = uint16(n)
	}
	if cfg.ConnConfig.Port != expectedPort {
		return nil, model.ErrInvalidRequest
	}
	q := u.Query()
	if name := q.Get("application_name"); name != "" && cfg.ConnConfig.RuntimeParams["application_name"] != name {
		return nil, model.ErrInvalidRequest
	}
	if timeout := q.Get("connect_timeout"); timeout != "" {
		n, _ := strconv.Atoi(timeout)
		if cfg.ConnConfig.ConnectTimeout != time.Duration(n)*time.Second {
			return nil, model.ErrInvalidRequest
		}
	}
	return cfg, nil
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
	poolCfg, err := localPoolConfig(cfg.DatabaseURL)
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
