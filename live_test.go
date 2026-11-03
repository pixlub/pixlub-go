package pixlub

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func liveClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv(APIKeyEnv) == "" || os.Getenv(BaseURLEnv) == "" {
		t.Skip("set PIXLUB_API_KEY and PIXLUB_BASE_URL to run the live tests")
	}
	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func TestLiveListsExecutableTools(t *testing.T) {
	client := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	tools, err := client.Tools.List(ctx, ListOptions{ExecutableOnly: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tools) == 0 {
		t.Fatalf("the catalog lists no executable tool")
	}
	for _, tool := range tools {
		if !tool.Executable {
			t.Errorf("%s is listed as executable only in the filter", tool.ID)
		}
	}
}

func TestLiveRunsFlip(t *testing.T) {
	client := liveClient(t)
	image := os.Getenv("PIXLUB_LIVE_IMAGE")
	if image == "" {
		t.Skip("set PIXLUB_LIVE_IMAGE to a small image to run a tool (this costs credits)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	result, err := client.Tools.Run(ctx, "flip", RunRequest{
		Image:  FromPath(image),
		Params: map[string]any{"axis": "h"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Image()) == 0 {
		t.Fatalf("the run returned no image")
	}
	if err := result.Save(filepath.Join(t.TempDir(), "flipped"+result.File().Extension()), false); err != nil {
		t.Fatalf("Save: %v", err)
	}
}
