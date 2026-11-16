package agent

import (
	"context"
	"encoding/json"
	"os"

	"github.com/theshield2594/thicket/internal/charimport"
)

// inDocker reports whether Thicket runs in a container, which changes how
// the import command is run.
var inDocker = func() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

func (e *Engine) toolImportGuide(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Platform string `json:"platform"`
		Memories *bool  `json:"memories"`
	}](args)
	if err != nil {
		return "", err
	}
	withMem := a.Memories == nil || *a.Memories
	return "Relay this to the user in your own voice, but keep any prompt and commands exactly as written, in code blocks:\n\n" +
		charimport.Guide(a.Platform, withMem, inDocker()), nil
}
