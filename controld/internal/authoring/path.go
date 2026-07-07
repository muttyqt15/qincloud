// path.go — the security boundary of the authoring surface.
//
// The web editor takes a note path from an untrusted HTTP form and turns it
// into a filesystem write inside the git working clone. That is exactly the
// kind of input that, unguarded, becomes a path-traversal write into
// controld's own code or the host. SafeRel is the single choke point: it
// confines every write to Markdown files under the vault's content directory
// and rejects anything that tries to climb out. Everything downstream trusts
// its output; nothing else parses the caller's path.
package authoring

import (
	"fmt"
	"path"
	"strings"
)

// maxRelLen bounds a note path — long enough for any real vault layout, short
// enough that a pathological input can't be used to probe the filesystem.
const maxRelLen = 255

// SafeRel validates a caller-supplied note path and returns it as a clean
// slash path relative to the content directory (never absolute, never
// containing ".."). It is the ONLY function allowed to interpret an untrusted
// path; a write must always be filepath.Join(root, contentSub, SafeRel(rel)).
//
// The guard fails LOUD rather than rescuing: an input that tries to climb out
// ("../x") or is absolute is rejected, not silently rewritten to something
// confined. Redundant-but-safe segments (".", an internal "sub/..") are the
// only normalisation allowed, via path.Clean. What survives is a non-empty,
// ".."-free, ".md" relative path.
func SafeRel(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("note path is empty")
	}
	if len(rel) > maxRelLen {
		return "", fmt.Errorf("note path too long (%d > %d)", len(rel), maxRelLen)
	}
	// Reject control bytes and backslashes outright rather than trusting Clean
	// to normalise them: a NUL truncates C-level path handling, and a
	// backslash is a separator on some systems and a literal on others — an
	// ambiguity a security guard should refuse, not resolve.
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("note path contains a null byte")
	}
	if strings.Contains(rel, `\`) {
		return "", fmt.Errorf("note path contains a backslash")
	}
	if path.IsAbs(rel) {
		return "", fmt.Errorf("note path %q must be relative, not absolute", rel)
	}

	clean := path.Clean(rel)
	if clean == "." {
		return "", fmt.Errorf("note path %q resolves to the content root, not a file", rel)
	}
	// After Clean, an escaping path is exactly one that still begins with "..".
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("note path %q escapes the content directory", rel)
	}
	if !strings.HasSuffix(clean, ".md") {
		return "", fmt.Errorf("note path %q must end in .md", rel)
	}
	return clean, nil
}
