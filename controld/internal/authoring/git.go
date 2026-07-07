// git.go — the real Committer: os/exec around the git binary in the controld
// image, operating on the working clone of the mirror.
//
// git is the source of truth; the box holds only a reconstructable clone
// (DR re-clones from the mirror). So a save must land back in the mirror, and
// the clone must stay current or a push would be rejected for divergence.
//
// Secret hygiene mirrors provisioning's rule (never a credential in argv): the
// push token is passed to git through the process ENV and read by an inline
// credential helper, so it appears in neither the command line (visible in
// `ps`) nor the persisted .git/config. The remote stays a plain https URL.
package authoring

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitConfig is the wiring an operator supplies to reach the mirror.
type GitConfig struct {
	Dir         string // the working clone directory (absolute)
	RepoURL     string // https mirror URL, for the first-boot clone
	AuthorName  string // commit author name
	AuthorEmail string // commit author email
	Token       string // a token with contents:write on the mirror; env-only, never argv
}

// gitTokenEnv is the env var the inline credential helper reads the push token
// from, so the token reaches git without ever appearing in argv or config.
const gitTokenEnv = "QC_GIT_TOKEN"

// credentialHelperArgs returns the `-c credential.helper=…` flag that makes git
// authenticate with x-access-token + $QC_GIT_TOKEN from the env.
func credentialHelperArgs() []string {
	helper := fmt.Sprintf(`!f() { echo "username=x-access-token"; echo "password=$%s"; }; f`, gitTokenEnv)
	return []string{"-c", "credential.helper=" + helper}
}

// EnsureClone makes cfg.Dir a working clone of the mirror if it isn't one yet —
// the box holds only a reconstructable clone, so a fresh box (or a fresh
// volume) clones on first serve. A directory that already has .git is left
// untouched; keeping it current is Pull's job.
func EnsureClone(ctx context.Context, cfg GitConfig) error {
	if _, err := os.Stat(filepath.Join(cfg.Dir, ".git")); err == nil {
		return nil
	}
	if cfg.RepoURL == "" {
		return fmt.Errorf("no clone at %s and no RepoURL to create one", cfg.Dir)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Dir), 0o755); err != nil {
		return fmt.Errorf("prepare clone parent: %w", err)
	}
	args := append(credentialHelperArgs(), "clone", cfg.RepoURL, cfg.Dir)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", gitTokenEnv+"="+cfg.Token)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clone %s: %w: %s", cfg.RepoURL, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

type gitCommitter struct {
	cfg GitConfig
}

// NewGitCommitter returns a Committer over the working clone. It does not
// clone or validate the directory — that is the box wiring's responsibility
// (see cmd/controld), which ensures the clone exists before serving.
func NewGitCommitter(cfg GitConfig) Committer {
	return &gitCommitter{cfg: cfg}
}

// run executes one git command in the clone. network=true adds the credential
// helper and disables any interactive prompt so a bad token fails fast instead
// of hanging.
func (g *gitCommitter) run(ctx context.Context, network bool, args ...string) (string, error) {
	full := []string{"-C", g.cfg.Dir}
	if network {
		full = append(full, credentialHelperArgs()...)
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	if network {
		cmd.Env = append(cmd.Env, gitTokenEnv+"="+g.cfg.Token)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (g *gitCommitter) Pull(ctx context.Context) error {
	_, err := g.run(ctx, true, "pull", "--ff-only")
	return err
}

func (g *gitCommitter) CommitPush(ctx context.Context, message string, addPaths ...string) error {
	if len(addPaths) == 0 {
		return fmt.Errorf("nothing to commit")
	}
	if _, err := g.run(ctx, false, append([]string{"add", "--"}, addPaths...)...); err != nil {
		return err
	}
	// A save that changes nothing (re-saving identical content) is a success,
	// not a failure — there is simply nothing to push.
	status, err := g.run(ctx, false, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) == "" {
		return nil
	}

	commitArgs := []string{
		"-c", "user.name=" + g.cfg.AuthorName,
		"-c", "user.email=" + g.cfg.AuthorEmail,
		"commit", "-m", message,
	}
	if _, err := g.run(ctx, false, commitArgs...); err != nil {
		return err
	}
	_, err = g.run(ctx, true, "push")
	return err
}
