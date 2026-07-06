// applock.go — deploy.Locker on a Postgres session-level advisory lock: one
// deploy/destroy per app at a time, across processes. Every CLI invocation is
// its own `docker exec` process, so the lock must live in shared state, and
// controld's own database is the one shared thing every invocation already
// reaches. Session-level (not transaction-level) so the lock spans the whole
// deploy without holding a transaction open, and it releases automatically
// when the session ends — a killed deploy cannot leave the app locked.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"qincloud/controld/internal/deploy"
)

type pgAppLock struct {
	dsn string
}

var _ deploy.Locker = (*pgAppLock)(nil)

// AcquireAppLock opens a dedicated connection — the lock lives exactly as
// long as this session — and try-locks: failing fast with a clear message
// beats silently queueing a second deploy behind a slow first one.
func (l *pgAppLock) AcquireAppLock(ctx context.Context, app string) (func(), error) {
	conn, err := pgx.Connect(ctx, l.dsn)
	if err != nil {
		return nil, fmt.Errorf("connect for app lock: %w", err)
	}
	var acquired bool
	err = conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext('qc-app:' || $1))`, app).Scan(&acquired)
	if err != nil {
		_ = conn.Close(ctx)
		return nil, fmt.Errorf("try app lock for %s: %w", app, err)
	}
	if !acquired {
		_ = conn.Close(ctx)
		return nil, fmt.Errorf("another deploy or destroy of %s is already running", app)
	}
	release := func() {
		// Closing the session is the release. Detached context: release also
		// runs on failure paths where the deploy context may already be dead,
		// and a lingering open session would keep the app locked until the
		// process exits.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}
	return release, nil
}
