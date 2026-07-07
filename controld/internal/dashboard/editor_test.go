// editor_test.go — the notes-editor routes against a fake authoring service,
// same style as dashboard_test.go: assert observable output/state, and observe
// SaveNote through a channel rather than a call count.
package dashboard

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"qincloud/controld/internal/authoring"
)

type savedNote struct{ rel, content string }

type fakeAuthoring struct {
	notes   []authoring.Note
	content string
	saved   chan savedNote // buffered; receives every SaveNote
	saveErr error
}

func newFakeAuthoring() *fakeAuthoring {
	return &fakeAuthoring{saved: make(chan savedNote, 4)}
}

func (f *fakeAuthoring) ListNotes() ([]authoring.Note, error) { return f.notes, nil }
func (f *fakeAuthoring) ReadNote(string) (string, error)      { return f.content, nil }
func (f *fakeAuthoring) SaveNote(_ context.Context, rel, content string) error {
	f.saved <- savedNote{rel, content}
	return f.saveErr
}

func newEditorServer(a Authoring) *http.ServeMux {
	s := New(&fakeStore{}, newFakeDeployer(), &fakeRuntime{})
	s.fastFail = 20 * time.Millisecond
	s.WithAuthoring(a)
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

func TestEditorListsNotes(t *testing.T) {
	a := newFakeAuthoring()
	a.notes = []authoring.Note{{Rel: "notes/alpha.md", Title: "Alpha"}}
	rec := get(t, newEditorServer(a), "/edit")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	wantContains(t, rec, "edit notes")
	wantContains(t, rec, "Alpha")
	wantContains(t, rec, "notes/alpha.md")
}

func TestEditorLoadsNoteContent(t *testing.T) {
	a := newFakeAuthoring()
	a.content = "# boom lives here"
	rec := get(t, newEditorServer(a), "/edit?path=notes/alpha.md")
	wantContains(t, rec, "# boom lives here")
}

func TestSaveNoteInvokesServiceAndConfirms(t *testing.T) {
	a := newFakeAuthoring()
	rec := post(t, newEditorServer(a), "/edit", url.Values{
		"path":    {"notes/hello.md"},
		"content": {"# boom"},
	}, true)

	select {
	case got := <-a.saved:
		if got.rel != "notes/hello.md" || got.content != "# boom" {
			t.Fatalf("SaveNote got %+v, want {notes/hello.md, # boom}", got)
		}
	case <-time.After(time.Second):
		t.Fatal("SaveNote was never called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	wantContains(t, rec, "published")
}

func TestSaveNoteRequiresPath(t *testing.T) {
	a := newFakeAuthoring()
	rec := post(t, newEditorServer(a), "/edit", url.Values{"content": {"x"}}, true)

	wantContains(t, rec, "path is required")
	select {
	case <-a.saved:
		t.Fatal("SaveNote was called despite an empty path")
	default:
	}
}

func TestSaveNoteRequiresHtmx(t *testing.T) {
	a := newFakeAuthoring()
	rec := post(t, newEditorServer(a), "/edit", url.Values{"path": {"notes/x.md"}}, false)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 without HX-Request", rec.Code)
	}
}

func TestEditorAbsentWithoutAuthoring(t *testing.T) {
	// The plain server (no WithAuthoring) must not expose /edit at all.
	rec := get(t, newTestServer(&fakeStore{}, newFakeDeployer()), "/edit")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the editor is disabled", rec.Code)
	}
}
