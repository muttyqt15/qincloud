// provision.go — per-app credentials on the SHARED data services: a Postgres
// role + database and a Redis ACL user, both named after the app. This is the
// one entry point for giving an app a database or a cache; the manual psql
// flow it replaces lives on in runbooks/data-services.md as the explanation.
//
// The isolation model: apps share one Postgres instance and one Redis
// instance, but each app authenticates as its own principal that can only
// reach its own data (its own database; its own `<app>:*` key prefix).
// tenant_db_net provides reachability, these credentials provide
// authorization.
//
// Provisioning runs `docker exec` into the data containers rather than using
// controld's own database connection — deliberately: controld's pg role is
// unprivileged by design (stack/data/initdb/01-controld.sh), and the exec
// capability (docker.sock) is one controld already holds. Secrets travel as
// exec-process env consumed by psql variables, and SQL goes over stdin, so
// neither a password nor an injectable name ever lands in argv or SQL text.
//
// A generated password is returned ONCE and never stored here. The caller
// passes it to the app via `deploy -env`, which stores it in the app spec —
// the same place every other app secret lives, backed up nightly.
package provision

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"qincloud/controld/internal/deploy"
)

// Container names are fixed in stack/data/compose.yml (backup.sh keys on the
// same names).
const (
	pgContainer    = "qincloud-postgres"
	redisContainer = "qincloud-redis"
)

// reserved are the platform's own principals. Load-bearing: without this,
// `provision -app qincloud -postgres -rotate` would ALTER the cluster
// superuser's password and `-app controld` would break CONTROLD_DSN. The
// grammar check alone cannot know these; the list must.
var reserved = []string{
	"default",   // redis default user (the platform password)
	"controld",  // control-plane role + database
	"qincloud",  // cluster superuser (POSTGRES_USER)
	"postgres",  // maintenance database
	"template0", // cluster templates
	"template1",
}

// Execer is the one capability provisioning needs: run a command inside a
// named container and capture its output. Implemented by dockerx.
type Execer interface {
	ExecCapture(ctx context.Context, container string, cmd, env []string, stdin string) (string, error)
}

type Provisioner struct {
	docker Execer
}

func New(docker Execer) *Provisioner {
	return &Provisioner{docker: docker}
}

// validateName gates every provisioning entry point: the app-name grammar
// (shared with deploy — the principal is named after the app) plus the
// reserved platform principals.
func validateName(app string) error {
	if err := deploy.ValidateAppName(app); err != nil {
		return err
	}
	if slices.Contains(reserved, app) {
		return fmt.Errorf("%q is a reserved platform name — provision under the app's own name", app)
	}
	return nil
}

// Postgres provisions app's role + database on the shared cluster and returns
// a ready-to-use DATABASE_URL. Fresh app: CREATE ROLE + CREATE DATABASE owned
// by it, with PUBLIC's default CONNECT revoked (without that revoke, any
// tenant role can connect to any database on the cluster). Existing role:
// refuses unless rotate, which sets a new password — the old DATABASE_URL
// stops working, so the caller must redeploy the app with the new URL.
func (p *Provisioner) Postgres(ctx context.Context, app string, rotate bool) (string, error) {
	if err := validateName(app); err != nil {
		return "", err
	}
	pw, err := newPassword()
	if err != nil {
		return "", err
	}

	// Every refusal happens BEFORE any write: role check, then database
	// ownership check, and only then the role mutation. Checking ownership
	// after re-keying the role would burn the app's live password on a
	// provision that then refuses — and every retry would fail the same way.
	roleExists, err := p.pgHasRow(ctx, app, `SELECT 1 FROM pg_roles WHERE rolname = :'name';`)
	if err != nil {
		return "", err
	}
	if roleExists && !rotate {
		return "", fmt.Errorf("postgres role %q already exists — rerun with -rotate to set a new password (then redeploy the app with the new DATABASE_URL)", app)
	}
	// CREATE DATABASE has no IF NOT EXISTS — check first, and when the name is
	// taken make sure it is OUR database (owner = the app role), not a
	// collision with something else on the cluster.
	owner, err := p.pgQueryOne(ctx, app, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = :'name';`)
	if err != nil {
		return "", err
	}
	if owner != "" && owner != app {
		return "", fmt.Errorf("database %q exists but is owned by %q — refusing to adopt it", app, owner)
	}

	if roleExists {
		err = p.pgRun(ctx, app, pw, `ALTER ROLE :"name" WITH LOGIN PASSWORD :'pw';`)
	} else {
		err = p.pgRun(ctx, app, pw, `CREATE ROLE :"name" LOGIN PASSWORD :'pw';`)
	}
	if err != nil {
		return "", err
	}

	if owner == "" {
		// psql -f runs statements one at a time, so CREATE DATABASE (which
		// cannot run in a transaction) and the revoke share one round-trip.
		err = p.pgRun(ctx, app, "",
			`CREATE DATABASE :"name" OWNER :"name";`+"\n"+
				`REVOKE CONNECT ON DATABASE :"name" FROM PUBLIC;`)
	} else {
		// Re-run on an existing database (rotate): converge the lockdown, so
		// databases provisioned before the revoke existed pick it up too.
		err = p.pgRun(ctx, app, "", `REVOKE CONNECT ON DATABASE :"name" FROM PUBLIC;`)
	}
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("postgresql://%s:%s@%s:5432/%s?sslmode=disable", app, pw, pgContainer, app), nil
}

// Redis provisions app's ACL user on the shared Redis and returns a
// ready-to-use REDIS_URL. The user is confined to the `<app>:*` key and
// channel prefix with admin/dangerous command classes removed, and only its
// password HASH crosses into the container (ACL's #sha256 form). ACL SAVE
// persists it to the aclfile so the user survives restarts; if that fails,
// the user is treated as not provisioned — a cache login that silently
// vanishes on the next redis restart is worse than a loud failure now.
func (p *Provisioner) Redis(ctx context.Context, app string, rotate bool) (string, error) {
	if err := validateName(app); err != nil {
		return "", err
	}
	pw, err := newPassword()
	if err != nil {
		return "", err
	}

	// redis-cli authenticates itself from the container's REDISCLI_AUTH env
	// (same mechanism scripts/backup.sh relies on) — no shell, no -a flag.
	users, err := p.docker.ExecCapture(ctx, redisContainer, []string{"redis-cli", "ACL", "USERS"}, nil, "")
	if err != nil {
		return "", fmt.Errorf("list redis users: %w", err)
	}
	if containsLine(users, app) && !rotate {
		return "", fmt.Errorf("redis user %q already exists — rerun with -rotate to set a new password (then redeploy the app with the new REDIS_URL)", app)
	}

	// reset first: a rotate REPLACES the user (old passwords and any drifted
	// rules gone), it never accumulates. One SETUSER call is atomic; rules
	// apply left to right. The trailing +info re-grants INFO out of
	// @dangerous — common clients send it during their connection handshake
	// (ioredis ready check, BullMQ/Sidekiq version detection; BullMQ verified
	// broken without it) — while CONFIG stays blocked: `CONFIG GET
	// requirepass` would hand a tenant the platform password.
	sum := sha256.Sum256([]byte(pw))
	if err := p.redisOK(ctx, "ACL", "SETUSER", app, "reset", "on", "#"+hex.EncodeToString(sum[:]),
		"~"+app+":*", "&"+app+":*", "+@all", "-@admin", "-@dangerous", "+info"); err != nil {
		return "", fmt.Errorf("create redis user %s: %w", app, err)
	}
	if err := p.redisOK(ctx, "ACL", "SAVE"); err != nil {
		return "", fmt.Errorf("persist redis user %s to the aclfile (is redis running with --aclfile? see stack/data/compose.yml): %w", app, err)
	}

	return fmt.Sprintf("redis://%s:%s@%s:6379/0", app, pw, redisContainer), nil
}

// --- postgres plumbing --------------------------------------------------------

// pgExec runs SQL inside the postgres container as the cluster superuser
// (the container's own POSTGRES_USER — local socket connections are trusted
// in the official image, the same way backup.sh dumps without a password).
// The app name and password ride in as exec env consumed by psql -v
// variables: SQL references them as :"name" (identifier) / :'pw' (literal),
// so quoting is psql's job, not string concatenation's. psql gotcha
// (runbooks/gotchas/data-services.md): -c does NOT interpolate -v variables —
// the SQL must arrive on stdin.
func (p *Provisioner) pgExec(ctx context.Context, app, pw, sql string) (string, error) {
	cmd := []string{"sh", "-c",
		`exec psql -U "$POSTGRES_USER" -d postgres -qAt -v ON_ERROR_STOP=1 -v name="$QC_NAME" -v pw="$QC_PW" -f -`}
	env := []string{"QC_NAME=" + app, "QC_PW=" + pw}
	out, err := p.docker.ExecCapture(ctx, pgContainer, cmd, env, sql+"\n")
	if err != nil {
		return "", fmt.Errorf("psql for %s: %w", app, err)
	}
	return out, nil
}

func (p *Provisioner) pgRun(ctx context.Context, app, pw, sql string) error {
	_, err := p.pgExec(ctx, app, pw, sql)
	return err
}

// pgQueryOne returns the single value a query yields, "" when it yields no
// rows (-qAt prints bare tuples only).
func (p *Provisioner) pgQueryOne(ctx context.Context, app, sql string) (string, error) {
	out, err := p.pgExec(ctx, app, "", sql)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// pgHasRow expects a `SELECT 1 …` probe: exactly "1" means the row exists.
// Anything else non-empty (stray warning text in the combined stream) is NOT
// treated as existing — the follow-up CREATE then fails loud on a real
// duplicate instead of this silently refusing on noise.
func (p *Provisioner) pgHasRow(ctx context.Context, app, sql string) (bool, error) {
	out, err := p.pgQueryOne(ctx, app, sql)
	return out == "1", err
}

// --- redis plumbing ------------------------------------------------------------

// redisOK runs one redis-cli command and requires the literal OK reply.
// redis-cli prints error replies to its output without always failing the
// exit code, so "did the command say OK" is the check with teeth.
func (p *Provisioner) redisOK(ctx context.Context, args ...string) error {
	out, err := p.docker.ExecCapture(ctx, redisContainer, append([]string{"redis-cli"}, args...), nil, "")
	if err != nil {
		return err
	}
	if reply := strings.TrimSpace(out); reply != "OK" {
		return fmt.Errorf("redis replied %q, expected OK", reply)
	}
	return nil
}

func containsLine(s, line string) bool {
	return slices.Contains(strings.Split(strings.TrimSpace(s), "\n"), line)
}

// newPassword returns 48 hex chars (24 random bytes) — URL-safe with no
// characters that need escaping in a DSN, a psql literal, or a redis ACL.
func newPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return hex.EncodeToString(b), nil
}
