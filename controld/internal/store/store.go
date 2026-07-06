// store.go — Postgres persistence for apps + deploy history, implementing
// deploy.Store. pgxpool with hand-written SQL; schema.sql is embedded and
// applied idempotently by Init. GetApp returns (nil, nil) when the app
// doesn't exist — callers distinguish absent from error.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"qincloud/controld/internal/deploy"
)

//go:embed schema.sql
var schema string

type Store struct {
	pool *pgxpool.Pool
}

var _ deploy.Store = (*Store)(nil)

// Connect opens a pgx pool to dsn (database "controld") and pings it, so a
// bad DSN fails at startup rather than on the first mid-deploy query.
func Connect(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Init applies the embedded schema. Idempotent by construction — every
// statement in schema.sql is IF NOT EXISTS.
func (s *Store) Init(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// uniqueViolation is Postgres SQLSTATE 23505 (unique_violation) — the only
// error code this adapter translates, so a named constant beats pulling in
// the pgerrcode module for one value.
const uniqueViolation = "23505"

// UpsertApp records the desired spec. It deliberately does NOT touch
// container_id: that column is owned by SetLiveContainer, and the state
// machine reads the previous live container through GetApp mid-deploy —
// clobbering it here would orphan the old container on redeploy.
// A host held by a different app violates apps_host_key and is translated
// into a "host already routed by app X" error; Deploy upserts before any
// container work, so the conflict fails the deploy up front.
func (s *Store) UpsertApp(ctx context.Context, spec deploy.AppSpec) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO apps (name, image, container_port, host)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET
			image          = EXCLUDED.image,
			container_port = EXCLUDED.container_port,
			host           = EXCLUDED.host,
			updated_at     = now()`,
		spec.Name, spec.Image, spec.ContainerPort, spec.Host)
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "apps_host_key" {
		return s.hostConflictError(ctx, spec)
	}
	return fmt.Errorf("upsert app %s: %w", spec.Name, err)
}

// hostConflictError names the app currently routing spec.Host, turning a raw
// unique-violation into an actionable error: on a control plane whose whole
// job is host→app routing, "duplicate key" is useless, "already routed by
// app X" tells the operator exactly what to destroy or rename.
func (s *Store) hostConflictError(ctx context.Context, spec deploy.AppSpec) error {
	var holder string
	err := s.pool.QueryRow(ctx, `SELECT name FROM apps WHERE host = $1`, spec.Host).Scan(&holder)
	if err != nil {
		// The conflict itself is certain — the unique index just fired —
		// so report it even when naming the holder fails.
		return fmt.Errorf("upsert app %s: host %s is already routed by another app (naming it failed: %v)",
			spec.Name, spec.Host, err)
	}
	return fmt.Errorf("upsert app %s: host %s is already routed by app %s", spec.Name, spec.Host, holder)
}

func (s *Store) CreateDeploy(ctx context.Context, app, image string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO deploys (app_name, image, status)
		VALUES ($1, $2, 'pending')
		RETURNING id`,
		app, image).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create deploy for %s: %w", app, err)
	}
	return id, nil
}

// SetDeployStatus records a transition; finished_at is stamped only when the
// deploy reaches a terminal state (live/failed), so started_at→finished_at
// spans the whole attempt.
func (s *Store) SetDeployStatus(ctx context.Context, id int64, status deploy.Status, errMsg string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE deploys SET
			status      = $2,
			error       = $3,
			finished_at = CASE WHEN $2 IN ('live', 'failed') THEN now() ELSE finished_at END
		WHERE id = $1`,
		id, string(status), errMsg)
	if err != nil {
		return fmt.Errorf("set deploy %d status %s: %w", id, status, err)
	}
	if tag.RowsAffected() == 0 {
		// A transition for a deploy that doesn't exist is a caller bug —
		// fail here, loud, instead of silently losing history.
		return fmt.Errorf("set deploy %d status %s: no such deploy", id, status)
	}
	return nil
}

func (s *Store) SetLiveContainer(ctx context.Context, app, containerID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE apps SET container_id = $2, updated_at = now() WHERE name = $1`,
		app, containerID)
	if err != nil {
		return fmt.Errorf("set live container for %s: %w", app, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set live container for %s: no such app", app)
	}
	return nil
}

// appColumns is the single source of column order for scanApp — GetApp and
// ListApps must never drift apart on what an App row looks like.
const appColumns = "name, image, container_port, host, container_id, updated_at"

func scanApp(row pgx.Row) (deploy.App, error) {
	var a deploy.App
	err := row.Scan(&a.Name, &a.Image, &a.ContainerPort, &a.Host, &a.ContainerID, &a.UpdatedAt)
	return a, err
}

func (s *Store) GetApp(ctx context.Context, app string) (*deploy.App, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+appColumns+` FROM apps WHERE name = $1`, app)
	a, err := scanApp(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // absent, not an error — the deploy.Store contract
	}
	if err != nil {
		return nil, fmt.Errorf("get app %s: %w", app, err)
	}
	return &a, nil
}

func (s *Store) ListApps(ctx context.Context) ([]deploy.App, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+appColumns+` FROM apps ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer rows.Close()
	var apps []deploy.App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, fmt.Errorf("scan app: %w", err)
		}
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	return apps, nil
}

// deployColumns is the single source of column order for scanDeploy, the
// same way appColumns is for scanApp.
const deployColumns = "id, app_name, image, status, error, started_at, finished_at"

func scanDeploy(row pgx.Row) (deploy.DeployRecord, error) {
	var d deploy.DeployRecord
	err := row.Scan(&d.ID, &d.AppName, &d.Image, &d.Status, &d.Error, &d.StartedAt, &d.FinishedAt)
	return d, err
}

// ListDeploys returns app's deploy history, newest first. Not part of
// deploy.Store — the state machine never reads history; the dashboard does.
func (s *Store) ListDeploys(ctx context.Context, app string, limit int) ([]deploy.DeployRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+deployColumns+` FROM deploys
		WHERE app_name = $1
		ORDER BY started_at DESC, id DESC
		LIMIT $2`,
		app, limit)
	if err != nil {
		return nil, fmt.Errorf("list deploys for %s: %w", app, err)
	}
	defer rows.Close()
	deploys := []deploy.DeployRecord{}
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan deploy: %w", err)
		}
		deploys = append(deploys, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deploys for %s: %w", app, err)
	}
	return deploys, nil
}

// LatestDeploys returns each app's most recent deploy, keyed by app name —
// the one-query projection behind every "current status" badge. id breaks
// same-instant started_at ties (identity column: higher id = later insert).
func (s *Store) LatestDeploys(ctx context.Context) (map[string]deploy.DeployRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (app_name) `+deployColumns+` FROM deploys
		ORDER BY app_name, started_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("latest deploys: %w", err)
	}
	defer rows.Close()
	latest := map[string]deploy.DeployRecord{}
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan deploy: %w", err)
		}
		latest[d.AppName] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("latest deploys: %w", err)
	}
	return latest, nil
}

// DeleteApp removes the app record; its deploy history goes with it
// (ON DELETE CASCADE). Deleting a name that doesn't exist is an error —
// on this control plane that is almost certainly a typo, not intent.
func (s *Store) DeleteApp(ctx context.Context, app string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM apps WHERE name = $1`, app)
	if err != nil {
		return fmt.Errorf("delete app %s: %w", app, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete app %s: no such app", app)
	}
	return nil
}

func (s *Store) Close() { s.pool.Close() }
