// Package sendfile implements the send_file MCP tool: deliver a file from the
// shared media directory to the user.
package sendfile

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Twinkle661/PhiRouter/internal/core"
)

type Tool struct {
	Media core.MediaStore
	Send  core.Inject
}

func (Tool) Name() string { return "send_file" }
func (Tool) Description() string {
	return "Send a file from the shared media directory to the user immediately."
}
func (Tool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}, "caption": map[string]string{"type": "string"}}, "required": []string{"path"}, "additionalProperties": false}
}
func (t Tool) Call(ctx context.Context, route string, raw json.RawMessage) (string, error) {
	var args struct{ Path, Caption string }
	if err := json.Unmarshal(raw, &args); err != nil || args.Path == "" {
		return "", invalidParams("Expected send_file with path and optional caption")
	}
	p, err := t.Media.Inspect(args.Path)
	if err != nil {
		return "", fmt.Errorf("invalid attachment: %w", err)
	}
	p.Caption = args.Caption
	if err = t.Send(ctx, core.Envelope{Route: route, Parts: []core.Part{p}}); err != nil {
		return "", err
	}
	return "File sent.", nil
}

type invalidParams string

func (e invalidParams) Error() string       { return string(e) }
func (e invalidParams) InvalidParams() bool { return true }
