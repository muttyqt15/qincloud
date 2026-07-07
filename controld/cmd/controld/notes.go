// notes.go — the wiring for the notes web editor (the authoring surface).
//
// The editor is opt-in by secret: if the git token file is present, controld
// builds an authoring.Service and the dashboard mounts /edit; if it's absent,
// the editor simply doesn't exist. Everything the editor needs beyond the
// control plane's normal capabilities — a working clone of the mirror, a token
// that can push to git and ghcr — is set up here.
//
// notesPublisher is the box-side equivalent of scripts/deploy-notes.sh: build
// the notes image from the working clone, push it to ghcr, and roll the app
// through the normal deploy state machine. Reusing deploy.Deploy means the
// editor inherits its one rule for free — a failed rebuild never takes the live
// site down.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"qincloud/controld/internal/authoring"
	"qincloud/controld/internal/deploy"
	"qincloud/controld/internal/dockerx"
)

// notesConfig is the fixed deployment identity of the notes app plus how to
// publish it. Defaults match scripts/deploy-notes.sh; every field is env-
// overridable so nothing app-specific is hard-compiled into the control plane.
type notesConfig struct {
	cloneDir   string // git working clone of the mirror (the whole repo)
	repoURL    string // https mirror URL, for the first-boot clone
	contentSub string // vault subdir, e.g. "learnings"
	dockerfile string // Dockerfile path relative to cloneDir
	imageRepo  string // ghcr image repo for the notes site
	host       string // public hostname
	appName    string // controld app name
	appPort    int    // container port (nginx)
	ghUser     string // ghcr username for the push
	token      string // token with contents:write + packages:write; git + ghcr
}

type notesPublisher struct {
	cfg      notesConfig
	docker   *dockerx.Client
	deployer *deploy.Deployer
}

// Publish rebuilds the notes image from the current working clone, pushes it to
// ghcr under a fresh immutable tag (plus :latest), and deploys that exact tag.
func (p *notesPublisher) Publish(ctx context.Context) error {
	tag := "v-" + time.Now().UTC().Format("20060102T150405Z")
	pinned := p.cfg.imageRepo + ":" + tag
	latest := p.cfg.imageRepo + ":latest"

	if err := p.docker.BuildImage(ctx, p.cfg.cloneDir, p.cfg.dockerfile, []string{pinned, latest}); err != nil {
		return fmt.Errorf("build notes image: %w", err)
	}
	if err := p.docker.PushImage(ctx, pinned, p.cfg.ghUser, p.cfg.token); err != nil {
		return fmt.Errorf("push %s: %w", pinned, err)
	}
	if err := p.docker.PushImage(ctx, latest, p.cfg.ghUser, p.cfg.token); err != nil {
		return fmt.Errorf("push %s: %w", latest, err)
	}
	// deploy the PINNED tag (not :latest) so the app rolls to the exact bytes
	// just built and its history records a real, redeployable version.
	spec := deploy.AppSpec{
		Name:          p.cfg.appName,
		Image:         pinned,
		ContainerPort: p.cfg.appPort,
		Host:          p.cfg.host,
	}
	if err := p.deployer.Deploy(ctx, spec); err != nil {
		return fmt.Errorf("deploy %s: %w", pinned, err)
	}
	return nil
}

// buildAuthoring constructs the notes editor if its git token secret is
// present, returning (nil, nil) when it isn't — the feature is enabled by
// dropping the secret, no separate flag. It ensures the working clone exists
// (first boot clones from the mirror) before returning a live service.
func buildAuthoring(ctx context.Context, dk *dockerx.Client, d *deploy.Deployer) (*authoring.Service, error) {
	tokenPath := envOr("QC_GIT_TOKEN_FILE", "/run/secrets/gh_token")
	raw, err := os.ReadFile(tokenPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no secret → editor disabled
		}
		return nil, fmt.Errorf("read git token %s: %w", tokenPath, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, nil
	}

	cfg := notesConfig{
		cloneDir:   envOr("QC_NOTES_CLONE", "/workspace"),
		repoURL:    envOr("QC_NOTES_REPO_URL", "https://github.com/muttyqt15/qincloud.git"),
		contentSub: envOr("QC_NOTES_CONTENT", "learnings"),
		dockerfile: envOr("QC_NOTES_DOCKERFILE", "sites/notes/Dockerfile"),
		imageRepo:  envOr("QC_NOTES_IMAGE", "ghcr.io/muttyqt15/qcloud-notes"),
		host:       envOr("QC_NOTES_HOST", "notes.sparboard.com"),
		appName:    envOr("QC_NOTES_APP", "notes"),
		appPort:    80,
		ghUser:     envOr("QC_GH_USER", "muttyqt15"),
		token:      token,
	}
	gitCfg := authoring.GitConfig{
		Dir:         cfg.cloneDir,
		RepoURL:     cfg.repoURL,
		AuthorName:  envOr("QC_GIT_AUTHOR_NAME", "qinny"),
		AuthorEmail: envOr("QC_GIT_AUTHOR_EMAIL", "hayaye69@gmail.com"),
		Token:       token,
	}
	if err := authoring.EnsureClone(ctx, gitCfg); err != nil {
		return nil, fmt.Errorf("ensure notes clone: %w", err)
	}
	pub := &notesPublisher{cfg: cfg, docker: dk, deployer: d}
	return authoring.New(cfg.cloneDir, cfg.contentSub, authoring.NewGitCommitter(gitCfg), pub), nil
}
