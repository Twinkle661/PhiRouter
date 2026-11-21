// Package plugins provides optional, explicitly configured hooks and triggers.
package plugins

import (
	"context"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"log/slog"
	"time"
)

type Prefix struct{ Text string }

func (p Prefix) OnInbound(_ context.Context, e core.Envelope) (core.Envelope, bool, error) {
	if p.Text != "" {
		parts := []core.Part{{Type: core.PartText, Text: p.Text}}
		e.Parts = append(parts, e.Parts...)
	}
	return e, false, nil
}
func (p Prefix) OnOutbound(_ context.Context, e core.Envelope) (core.Envelope, bool, error) {
	return e, false, nil
}
func (p Prefix) OnEvent(context.Context, string, core.Event) error { return nil }
func Ticker(ctx context.Context, interval time.Duration, route, text string, inject core.Inject) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := inject(ctx, core.Envelope{Route: route, Parts: []core.Part{{Type: core.PartText, Text: text}}}); err != nil {
				slog.Error("ticker injection failed", "error", err)
			}
		}
	}
}
