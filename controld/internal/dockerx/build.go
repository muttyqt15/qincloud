// build.go — image build + push, the two capabilities the notes web editor
// needs that a plain deploy does not. controld builds the notes image from the
// git working clone on the box, pushes it to ghcr, then deploys it through the
// normal state machine — so a browser edit publishes without a laptop.
//
// Both operations run on the daemon (via docker.sock); controld only streams
// the build context in and the progress out, so its own 128MB ceiling is never
// the build's limit. Errors for both arrive as JSON messages INSIDE the
// progress stream, so the stream must be drained AND parsed, not discarded —
// the same rule as Pull.
package dockerx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/pkg/jsonmessage"
	archive "github.com/moby/go-archive"
)

// BuildImage builds an image from contextDir (a directory tree, e.g. the repo
// clone) using the Dockerfile at dockerfileRel (relative to contextDir), and
// tags it. It uses the classic builder (Version 1): the notes Dockerfile is
// classic-compatible (plain multi-stage, no BuildKit-only mounts), which avoids
// standing up a BuildKit session from Go. .git is excluded from the context so
// a multi-megabyte history isn't tarred to the daemon on every publish.
func (c *Client) BuildImage(ctx context.Context, contextDir, dockerfileRel string, tags []string) error {
	tar, err := archive.TarWithOptions(contextDir, &archive.TarOptions{
		ExcludePatterns: []string{".git"},
	})
	if err != nil {
		return fmt.Errorf("tar build context %s: %w", contextDir, err)
	}
	defer tar.Close()

	resp, err := c.api.ImageBuild(ctx, tar, build.ImageBuildOptions{
		Tags:       tags,
		Dockerfile: dockerfileRel,
		Remove:     true,
		Version:    build.BuilderV1,
	})
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}
	defer resp.Body.Close()
	if err := jsonmessage.DisplayJSONMessagesStream(resp.Body, io.Discard, 0, false, nil); err != nil {
		return fmt.Errorf("build image: %w", err)
	}
	return nil
}

// PushImage pushes ref to its registry, authenticating as username with token.
// The credential is base64-JSON in the push options — the SDK's own mechanism —
// so it never appears on a command line.
func (c *Client) PushImage(ctx context.Context, ref, username, token string) error {
	authJSON, err := json.Marshal(registry.AuthConfig{Username: username, Password: token})
	if err != nil {
		return fmt.Errorf("encode registry auth: %w", err)
	}
	progress, err := c.api.ImagePush(ctx, ref, image.PushOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString(authJSON),
	})
	if err != nil {
		return fmt.Errorf("push %s: %w", ref, err)
	}
	defer progress.Close()
	if err := jsonmessage.DisplayJSONMessagesStream(progress, io.Discard, 0, false, nil); err != nil {
		return fmt.Errorf("push %s: %w", ref, err)
	}
	return nil
}
