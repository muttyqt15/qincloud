package authoring

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGit records the git effects so tests assert what was staged/committed and
// in what order relative to the write and publish, never spying on call counts
// for their own sake.
type fakeGit struct {
	pulls   int
	commits []commitCall
	pullErr error
	pushErr error
}

type commitCall struct {
	message string
	paths   []string
}

func (f *fakeGit) Pull(ctx context.Context) error {
	f.pulls++
	return f.pullErr
}

func (f *fakeGit) CommitPush(ctx context.Context, message string, addPaths ...string) error {
	if f.pushErr != nil {
		return f.pushErr
	}
	f.commits = append(f.commits, commitCall{message: message, paths: addPaths})
	return nil
}

type fakePublisher struct {
	published int
	err       error
}

func (f *fakePublisher) Publish(ctx context.Context) error {
	f.published++
	return f.err
}

// newService wires a Service over a temp repo with a learnings/ content dir.
func newService(t *testing.T) (*Service, string, *fakeGit, *fakePublisher) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "learnings", "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := &fakeGit{}
	pub := &fakePublisher{}
	return New(root, "learnings", git, pub), root, git, pub
}

func TestSaveNote_WritesCommitsAndPublishes(t *testing.T) {
	svc, root, git, pub := newService(t)

	if err := svc.SaveNote(context.Background(), "notes/hello.md", "# boom\n"); err != nil {
		t.Fatalf("SaveNote: %v", err)
	}

	// The file landed under learnings/ with the content.
	got, err := os.ReadFile(filepath.Join(root, "learnings", "notes", "hello.md"))
	if err != nil {
		t.Fatalf("note not written: %v", err)
	}
	if string(got) != "# boom\n" {
		t.Fatalf("note content = %q, want %q", got, "# boom\n")
	}

	// It was synced first, then committed with the repo-relative path, then published.
	if git.pulls != 1 {
		t.Errorf("pulls = %d, want 1 (must sync before writing)", git.pulls)
	}
	if len(git.commits) != 1 {
		t.Fatalf("commits = %d, want 1", len(git.commits))
	}
	if want := "learnings/notes/hello.md"; git.commits[0].paths[0] != want {
		t.Errorf("committed path = %q, want %q", git.commits[0].paths[0], want)
	}
	if pub.published != 1 {
		t.Errorf("published = %d, want 1", pub.published)
	}
}

func TestSaveNote_RejectsUnsafePathBeforeAnyEffect(t *testing.T) {
	svc, root, git, pub := newService(t)

	err := svc.SaveNote(context.Background(), "../../secrets.md", "x")
	if err == nil {
		t.Fatal("SaveNote with a climbing path should fail")
	}
	// Nothing outside the content dir was written, and no git/publish ran.
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "secrets.md")); !os.IsNotExist(statErr) {
		t.Error("a file escaped the content directory")
	}
	if git.pulls != 0 || len(git.commits) != 0 || pub.published != 0 {
		t.Errorf("effects ran on a rejected path: pulls=%d commits=%d published=%d", git.pulls, len(git.commits), pub.published)
	}
}

func TestSaveNote_RejectsOversizeContent(t *testing.T) {
	svc, _, git, pub := newService(t)
	big := strings.Repeat("a", maxNoteBytes+1)
	if err := svc.SaveNote(context.Background(), "notes/big.md", big); err == nil {
		t.Fatal("oversize note should be rejected")
	}
	if git.pulls != 0 || pub.published != 0 {
		t.Error("effects ran on an oversize note")
	}
}

func TestSaveNote_PushFailureSkipsPublish(t *testing.T) {
	svc, _, git, pub := newService(t)
	git.pushErr = errors.New("remote rejected")

	err := svc.SaveNote(context.Background(), "notes/hello.md", "# hi")
	if err == nil {
		t.Fatal("SaveNote should fail when the push fails")
	}
	if pub.published != 0 {
		t.Error("published despite a failed push — the site could diverge from git")
	}
}

func TestSaveNote_PublishFailureNamesTheStage(t *testing.T) {
	svc, _, _, pub := newService(t)
	pub.err = errors.New("build blew up")

	err := svc.SaveNote(context.Background(), "notes/hello.md", "# hi")
	if err == nil {
		t.Fatal("SaveNote should surface a publish failure")
	}
	if !strings.Contains(err.Error(), "saved to git") {
		t.Errorf("publish-failure error should reassure the change is saved, got: %v", err)
	}
}

func TestReadAndListNotes(t *testing.T) {
	svc, root, _, _ := newService(t)
	write := func(rel, content string) {
		p := filepath.Join(root, "learnings", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("notes/a.md", "---\ntitle: Alpha\n---\nbody")
	write("notes/b.md", "# Bravo heading\ntext")
	write("notes/c.md", "no title here")

	got, err := svc.ReadNote("notes/a.md")
	if err != nil {
		t.Fatalf("ReadNote: %v", err)
	}
	if !strings.Contains(got, "title: Alpha") {
		t.Errorf("ReadNote returned %q", got)
	}

	notes, err := svc.ListNotes()
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	titles := map[string]string{}
	for _, n := range notes {
		titles[n.Rel] = n.Title
	}
	if titles["notes/a.md"] != "Alpha" {
		t.Errorf("title from frontmatter = %q, want Alpha", titles["notes/a.md"])
	}
	if titles["notes/b.md"] != "Bravo heading" {
		t.Errorf("title from heading = %q, want 'Bravo heading'", titles["notes/b.md"])
	}
	if titles["notes/c.md"] != "c.md" {
		t.Errorf("title fallback = %q, want filename c.md", titles["notes/c.md"])
	}
}

func TestReadNote_RejectsUnsafePath(t *testing.T) {
	svc, _, _, _ := newService(t)
	if _, err := svc.ReadNote("../../etc/passwd"); err == nil {
		t.Fatal("ReadNote should reject a climbing path")
	}
}
