// provision_test.go — the Provisioner's observable output is the exact
// command sequence it sends into the data containers, so that is what these
// tests pin: which SQL/ACL commands run, that secrets travel only as exec env
// (never in argv or SQL text), and that refuse/rotate paths stop where they
// should. The Execer fake replies from a script and fails the test on any
// call beyond it.
package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

type call struct {
	container string
	cmd       []string
	env       []string
	stdin     string
}

type reply struct {
	out string
	err error
}

type fakeExecer struct {
	t       *testing.T
	calls   []call
	replies []reply
}

func (f *fakeExecer) ExecCapture(_ context.Context, container string, cmd, env []string, stdin string) (string, error) {
	f.t.Helper()
	f.calls = append(f.calls, call{container, cmd, env, stdin})
	if len(f.replies) == 0 {
		f.t.Fatalf("unexpected exec call #%d: %v (stdin %q)", len(f.calls), cmd, stdin)
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r.out, r.err
}

func newFake(t *testing.T, replies ...reply) *fakeExecer {
	return &fakeExecer{t: t, replies: replies}
}

// passwordOf pulls the generated password back out of the returned URL — the
// only place it is ever surfaced.
func passwordOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("returned URL does not parse: %v", err)
	}
	pw, ok := u.User.Password()
	if !ok {
		t.Fatalf("returned URL carries no password: %s", rawURL)
	}
	return pw
}

func TestPostgres_FreshApp(t *testing.T) {
	f := newFake(t,
		reply{out: ""}, // role exists? no
		reply{out: ""}, // database owner? none
		reply{out: ""}, // CREATE ROLE
		reply{out: ""}, // CREATE DATABASE + REVOKE
	)
	got, err := New(f).Postgres(context.Background(), "blog", false)
	if err != nil {
		t.Fatalf("Postgres() error: %v", err)
	}

	wantURL := regexp.MustCompile(`^postgresql://blog:[0-9a-f]{48}@qincloud-postgres:5432/blog\?sslmode=disable$`)
	if !wantURL.MatchString(got) {
		t.Errorf("DATABASE_URL = %q, want match %s", got, wantURL)
	}
	if len(f.calls) != 4 {
		t.Fatalf("got %d exec calls, want 4", len(f.calls))
	}
	for i, c := range f.calls {
		if c.container != "qincloud-postgres" {
			t.Errorf("call %d hit container %q, want qincloud-postgres", i, c.container)
		}
	}

	pw := passwordOf(t, got)
	create := f.calls[2]
	if !strings.Contains(create.stdin, `CREATE ROLE :"name" LOGIN PASSWORD :'pw';`) {
		t.Errorf("create-role stdin = %q, want psql-variable CREATE ROLE", create.stdin)
	}
	// The password reaches psql as exec env only — never inside the SQL text
	// and never in argv.
	if strings.Contains(create.stdin, pw) {
		t.Errorf("password leaked into SQL text: %q", create.stdin)
	}
	if strings.Contains(strings.Join(create.cmd, " "), pw) {
		t.Errorf("password leaked into argv: %v", create.cmd)
	}
	wantEnv := []string{"QC_NAME=blog", "QC_PW=" + pw}
	for _, w := range wantEnv {
		found := false
		for _, e := range create.env {
			if e == w {
				found = true
			}
		}
		if !found {
			t.Errorf("create-role env %v missing %q", create.env, w)
		}
	}

	createDB := f.calls[3].stdin
	if !strings.Contains(createDB, `CREATE DATABASE :"name" OWNER :"name";`) {
		t.Errorf("create-db stdin = %q, want CREATE DATABASE", createDB)
	}
	if !strings.Contains(createDB, `REVOKE CONNECT ON DATABASE :"name" FROM PUBLIC;`) {
		t.Errorf("create-db stdin = %q, want the PUBLIC connect revoke", createDB)
	}
}

func TestPostgres_ExistingRoleRefusesWithoutRotate(t *testing.T) {
	f := newFake(t, reply{out: "1\n"}) // role exists? yes — and nothing more
	_, err := New(f).Postgres(context.Background(), "blog", false)
	if err == nil || !strings.Contains(err.Error(), "-rotate") {
		t.Fatalf("err = %v, want refusal pointing at -rotate", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("got %d exec calls, want 1 (no mutation after the refusal)", len(f.calls))
	}
}

func TestPostgres_RotateAltersExistingRole(t *testing.T) {
	f := newFake(t,
		reply{out: "1\n"},    // role exists? yes
		reply{out: "blog\n"}, // database owner? blog
		reply{out: ""},       // ALTER ROLE
		reply{out: ""},       // converge the revoke
	)
	got, err := New(f).Postgres(context.Background(), "blog", true)
	if err != nil {
		t.Fatalf("Postgres(rotate) error: %v", err)
	}
	if !strings.Contains(f.calls[2].stdin, `ALTER ROLE :"name" WITH LOGIN PASSWORD :'pw';`) {
		t.Errorf("rotate stdin = %q, want ALTER ROLE", f.calls[2].stdin)
	}
	last := f.calls[3].stdin
	if strings.Contains(last, "CREATE DATABASE") {
		t.Errorf("rotate must not re-create the existing database: %q", last)
	}
	if !strings.Contains(last, `REVOKE CONNECT ON DATABASE :"name" FROM PUBLIC;`) {
		t.Errorf("rotate stdin = %q, want the convergent revoke", last)
	}
	if pw := passwordOf(t, got); !strings.Contains(strings.Join(f.calls[2].env, "\n"), "QC_PW="+pw) {
		t.Errorf("rotated password not in exec env")
	}
}

func TestPostgres_RefusesForeignDatabaseBeforeAnyWrite(t *testing.T) {
	f := newFake(t,
		reply{out: ""},           // role exists? no
		reply{out: "qincloud\n"}, // database owner? someone else
	)
	_, err := New(f).Postgres(context.Background(), "blog", false)
	if err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("err = %v, want owned-by refusal", err)
	}
	// The refusal must precede EVERY write: no role was created or re-keyed,
	// so a refused provision never burns the app's live password.
	if len(f.calls) != 2 {
		t.Errorf("got %d exec calls, want 2 (checks only, no writes)", len(f.calls))
	}
	for i, c := range f.calls {
		if strings.Contains(c.stdin, "CREATE") || strings.Contains(c.stdin, "ALTER") {
			t.Errorf("call %d mutated despite the refusal: %q", i, c.stdin)
		}
	}
}

func TestRedis_FreshApp(t *testing.T) {
	f := newFake(t,
		reply{out: "default\n"}, // ACL USERS
		reply{out: "OK\n"},      // ACL SETUSER
		reply{out: "OK\n"},      // ACL SAVE
	)
	got, err := New(f).Redis(context.Background(), "blog", false)
	if err != nil {
		t.Fatalf("Redis() error: %v", err)
	}

	wantURL := regexp.MustCompile(`^redis://blog:[0-9a-f]{48}@qincloud-redis:6379/0$`)
	if !wantURL.MatchString(got) {
		t.Errorf("REDIS_URL = %q, want match %s", got, wantURL)
	}
	for i, c := range f.calls {
		if c.container != "qincloud-redis" {
			t.Errorf("call %d hit container %q, want qincloud-redis", i, c.container)
		}
	}

	sum := sha256.Sum256([]byte(passwordOf(t, got)))
	// +info is deliberate and last (rules apply left to right): common
	// clients send INFO during their connection handshake and @dangerous
	// contains it. CONFIG must stay blocked.
	wantSetuser := []string{
		"redis-cli", "ACL", "SETUSER", "blog", "reset", "on", "#" + hex.EncodeToString(sum[:]),
		"~blog:*", "&blog:*", "+@all", "-@admin", "-@dangerous", "+info",
	}
	gotSetuser := f.calls[1].cmd
	if strings.Join(gotSetuser, " ") != strings.Join(wantSetuser, " ") {
		t.Errorf("SETUSER argv:\n got  %v\n want %v", gotSetuser, wantSetuser)
	}
	if want := "redis-cli ACL SAVE"; strings.Join(f.calls[2].cmd, " ") != want {
		t.Errorf("persist argv = %v, want %q", f.calls[2].cmd, want)
	}
}

func TestRedis_ExistingUserRefusesWithoutRotate(t *testing.T) {
	f := newFake(t, reply{out: "default\nblog\n"})
	_, err := New(f).Redis(context.Background(), "blog", false)
	if err == nil || !strings.Contains(err.Error(), "-rotate") {
		t.Fatalf("err = %v, want refusal pointing at -rotate", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("got %d exec calls, want 1 (no SETUSER after the refusal)", len(f.calls))
	}
}

func TestRedis_RotateReplacesExistingUser(t *testing.T) {
	f := newFake(t,
		reply{out: "default\nblog\n"},
		reply{out: "OK\n"},
		reply{out: "OK\n"},
	)
	if _, err := New(f).Redis(context.Background(), "blog", true); err != nil {
		t.Fatalf("Redis(rotate) error: %v", err)
	}
	// reset inside the single SETUSER is what makes rotate a replace.
	if argv := strings.Join(f.calls[1].cmd, " "); !strings.Contains(argv, "SETUSER blog reset on") {
		t.Errorf("rotate SETUSER argv = %q, want reset-then-on", argv)
	}
}

func TestRedis_FailedPersistIsAnError(t *testing.T) {
	f := newFake(t,
		reply{out: "default\n"},
		reply{out: "OK\n"},
		reply{out: "ERR This Redis instance is not configured to use an ACL file\n"},
	)
	_, err := New(f).Redis(context.Background(), "blog", false)
	if err == nil || !strings.Contains(err.Error(), "aclfile") {
		t.Fatalf("err = %v, want aclfile failure", err)
	}
}

func TestValidateName_RejectsReservedAndInvalid(t *testing.T) {
	tests := []struct {
		name string
		app  string
	}{
		{"redis default user", "default"},
		{"control plane role", "controld"},
		{"cluster superuser", "qincloud"},
		{"maintenance database", "postgres"},
		{"template database", "template1"},
		{"uppercase", "Blog"},
		{"underscore", "my_app"},
		{"empty", ""},
		{"too long", strings.Repeat("a", 33)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t) // any exec call fails the test
			if _, err := New(f).Postgres(context.Background(), tt.app, true); err == nil {
				t.Errorf("Postgres(%q) accepted, want rejection", tt.app)
			}
			if _, err := New(f).Redis(context.Background(), tt.app, true); err == nil {
				t.Errorf("Redis(%q) accepted, want rejection", tt.app)
			}
			if len(f.calls) != 0 {
				t.Errorf("%q reached the data containers: %v", tt.app, f.calls)
			}
		})
	}
}
