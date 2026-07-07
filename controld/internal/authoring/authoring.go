// authoring.go — the notes authoring service: the logic behind editing the
// learnings vault from the browser and publishing the result.
//
// One linear path per save:
//
//	SafeRel → git.Pull → write file → git.CommitPush → publisher.Publish
//
// The two side-effecting boundaries — git (write the change back to the mirror)
// and publish (rebuild + redeploy the static site) — are interfaces so this
// logic is testable against fakes without a repo or a Docker daemon. The
// filesystem is exercised directly against a temp dir in tests; it is not a
// mockable seam, it is the thing under test.
//
// git is the source of truth (the box holds only a reconstructable clone), so
// a save is not "done" until it is committed and pushed; publish then makes it
// visible. Ordering matters: if the push fails, the site is never rebuilt from
// an un-pushed change, so git and the live site never disagree.
package authoring

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxNoteBytes caps a single note. Generous for prose; small enough that the
// editor can't be used to write a huge file into the repo.
const maxNoteBytes = 1 << 20 // 1 MiB

// Committer is the git side effect: keep the working clone current with the
// mirror, and persist a change back to it. Implemented by internal/authoring's
// gitCommitter (os/exec around the git binary); faked in tests.
type Committer interface {
	// Pull fast-forwards the working clone to the mirror. It must fail rather
	// than merge — a divergence is a real conflict the operator should see.
	Pull(ctx context.Context) error
	// CommitPush stages addPaths (repo-relative), commits them with message,
	// and pushes to the mirror. A no-op change (nothing staged) is not an error.
	CommitPush(ctx context.Context, message string, addPaths ...string) error
}

// Publisher rebuilds and redeploys the notes site from the current working
// clone. Slow — a Quartz image build — so callers run SaveNote off the request
// path. Implemented by wiring in cmd/controld (build → push → deploy); faked in
// tests.
type Publisher interface {
	Publish(ctx context.Context) error
}

// Service edits Markdown notes in a git working clone and publishes them.
type Service struct {
	root       string // absolute path of the git working clone (the whole repo)
	contentSub string // subdir under root holding the vault, e.g. "learnings"
	git        Committer
	publisher  Publisher
}

// New builds a Service over the working clone at root, with the vault under
// contentSub (e.g. "learnings"). Both git effects and publishing are injected.
func New(root, contentSub string, git Committer, publisher Publisher) *Service {
	return &Service{root: root, contentSub: contentSub, git: git, publisher: publisher}
}

// contentDir is the absolute directory every note lives under. Confinement to
// it is what SafeRel guarantees; joining here is what enforces it.
func (s *Service) contentDir() string {
	return filepath.Join(s.root, s.contentSub)
}

// Note is one Markdown file in the vault, as listed for the editor.
type Note struct {
	Rel     string    // slash path relative to contentSub, e.g. "notes/foo.md"
	Title   string    // frontmatter title, else first heading, else the filename
	ModTime time.Time // filesystem mtime, for "most recently edited first"
}

// ListNotes returns every .md file under the content directory, most recently
// modified first — the picker the editor shows.
func (s *Service) ListNotes() ([]Note, error) {
	dir := s.contentDir()
	var notes []Note
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		notes = append(notes, Note{
			Rel:     filepath.ToSlash(rel),
			Title:   titleOf(p, info.Name()),
			ModTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].ModTime.After(notes[j].ModTime) })
	return notes, nil
}

// ReadNote returns the content of one note. The path is validated before it is
// ever joined to the filesystem.
func (s *Service) ReadNote(rel string) (string, error) {
	safe, err := SafeRel(rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(s.contentDir(), filepath.FromSlash(safe)))
	if err != nil {
		return "", fmt.Errorf("read note %q: %w", safe, err)
	}
	return string(b), nil
}

// SaveNote writes content to the note at rel, commits and pushes it to the
// mirror, then rebuilds and redeploys the site. It is the whole save path; the
// HTTP handler runs it off the request (a publish takes a while).
//
// The steps are ordered so git and the live site can never disagree: pull to
// be current, write, commit+push (git is now the truth), and only then publish.
// A failure at any step stops the ones after it — most importantly, a failed
// push never reaches publish, so the site is never built from a change the
// mirror doesn't have.
func (s *Service) SaveNote(ctx context.Context, rel, content string) error {
	safe, err := SafeRel(rel)
	if err != nil {
		return err
	}
	if len(content) > maxNoteBytes {
		return fmt.Errorf("note is too large (%d > %d bytes)", len(content), maxNoteBytes)
	}

	if err := s.git.Pull(ctx); err != nil {
		return fmt.Errorf("sync working clone: %w", err)
	}

	abs := filepath.Join(s.contentDir(), filepath.FromSlash(safe))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("create note directory: %w", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write note %q: %w", safe, err)
	}

	repoRel := path.Join(s.contentSub, safe) // repo-relative, slash form for git
	msg := fmt.Sprintf("notes: edit %s via web editor", safe)
	if err := s.git.CommitPush(ctx, msg, repoRel); err != nil {
		return fmt.Errorf("commit note to git: %w", err)
	}

	if err := s.publisher.Publish(ctx); err != nil {
		// The change is safely in git; only the rebuild failed. Say exactly
		// that so the operator knows the content is not lost and can retry the
		// publish, not the edit.
		return fmt.Errorf("saved to git, but rebuilding the site failed: %w", err)
	}
	return nil
}

// titleOf extracts a display title: the frontmatter `title:` if present, else
// the first Markdown heading, else the filename. Best-effort and cheap — it
// only reads the head of the file. A missing/garbled file falls back to the
// name rather than failing the whole listing.
func titleOf(absPath, filename string) string {
	b, err := os.ReadFile(absPath)
	if err != nil {
		return filename
	}
	lines := strings.Split(string(b), "\n")
	inFrontmatter := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if i == 0 && trimmed == "---" {
			inFrontmatter = true
			continue
		}
		if inFrontmatter {
			if trimmed == "---" {
				inFrontmatter = false
				continue
			}
			if rest, ok := strings.CutPrefix(trimmed, "title:"); ok {
				if t := unquote(strings.TrimSpace(rest)); t != "" {
					return t
				}
			}
			continue
		}
		if h, ok := strings.CutPrefix(trimmed, "# "); ok {
			if h = strings.TrimSpace(h); h != "" {
				return h
			}
		}
	}
	return filename
}

// unquote strips one layer of matching quotes from a frontmatter scalar.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
