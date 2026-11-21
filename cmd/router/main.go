package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/Twinkle661/PhiRouter/internal/api"
	"github.com/Twinkle661/PhiRouter/internal/config"
	"github.com/Twinkle661/PhiRouter/internal/core"
	"github.com/Twinkle661/PhiRouter/internal/ingress"
	"github.com/Twinkle661/PhiRouter/internal/mcp"
	"github.com/Twinkle661/PhiRouter/internal/media"
	"github.com/Twinkle661/PhiRouter/internal/router"
	"github.com/Twinkle661/PhiRouter/plugins"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("router stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "config.yaml", "YAML configuration path")
	dotenv := flag.String("env", ".env", "dotenv path")
	health := flag.String("healthcheck", "", "check health URL and exit")
	flag.Parse()
	if *health != "" {
		client := http.Client{Timeout: 3 * time.Second}
		res, err := client.Get(*health)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return fmt.Errorf("healthcheck status %d", res.StatusCode)
		}
		return nil
	}
	c, err := config.Load(*path, *dotenv, registries())
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if c.Debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	store, err := media.New(c.Media.Dir)
	if err != nil {
		return err
	}
	defer store.Close()
	backends, policies, err := buildBackends(c, store)
	if err != nil {
		return err
	}
	frontends, owners, webhooks, err := buildFrontends(c, store)
	if err != nil {
		return err
	}
	var hooks []core.Hook
	for _, p := range c.Plugins {
		if p.Type == "prefix" {
			hooks = append(hooks, plugins.Prefix{Text: p.Prefix})
		}
	}
	var routes []router.Route
	for _, r := range c.Routes {
		routes = append(routes, router.Route{From: r.From, To: r.To})
	}
	timestamp := router.TimestampOptions{Enabled: c.Timestamp.Enabled, Location: c.Timestamp.Location, Format: c.Timestamp.Format}
	rt := router.New(router.Options{Timestamp: timestamp, Routes: routes, Owners: owners, Separators: c.Stream.Separators, ShowTools: c.Stream.ShowToolCalls.Enabled, ToolDetail: c.Stream.ShowToolCalls.Detailed, ToolDetailLimit: c.Stream.ShowToolCalls.MaxDetailLength, QueueLimit: c.QueueLimit, BusyPolicy: policies, Hooks: hooks}, frontends, backends)
	apiListener, err := net.Listen("tcp", c.API.Listen)
	if err != nil {
		return err
	}
	defer apiListener.Close()
	mcpListener, err := net.Listen("tcp", c.MCP.Listen)
	if err != nil {
		return err
	}
	defer mcpListener.Close()
	tools, err := buildTools(c, store, rt.Send)
	if err != nil {
		return err
	}
	mcpRoutes := map[string]bool{}
	for key := range backends {
		mcpRoutes[key] = true
	}
	servers := []*http.Server{{Handler: api.New(c.API.Token, store.Validate, rt.Inject), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second}, {Handler: mcp.New(mcp.Options{Token: c.MCP.Token, Routes: mcpRoutes, Tools: tools}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second}}
	listeners := []net.Listener{apiListener, mcpListener}
	// The ingress listener only exists when some frontend declares a webhook path.
	if len(webhooks) > 0 {
		handlers := map[string]http.Handler{}
		for p, f := range webhooks {
			handlers[p] = f.Ingress()
		}
		ingressListener, err := net.Listen("tcp", c.Ingress.Listen)
		if err != nil {
			return err
		}
		defer ingressListener.Close()
		listeners = append(listeners, ingressListener)
		servers = append(servers, &http.Server{Handler: ingress.New(handlers), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second})
	} else if c.Ingress.Listen != "" {
		slog.Warn("ingress.listen is set but no frontend declares an ingress path; not listening")
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	routerCtx, cancelRouter := context.WithCancel(context.Background())
	defer cancelRouter()
	backendCtx, cancelBackends := context.WithCancel(context.Background())
	defer cancelBackends()
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	launch := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				select {
				case errs <- err:
				default:
				}
			}
		}()
	}
	lifecycle := func(text string) {
		for _, name := range c.LifecycleNotify {
			notifyLifecycle(frontends[name], owners[name], text)
		}
	}
	lifecycle("Server started ₍^ >ヮ<^₎ .ᐟ.ᐟ\n\nPhiRouter is online. Backends are connecting; messages will queue until ready.")
	routerDone := make(chan struct{})
	go func() { defer close(routerDone); _ = rt.Run(routerCtx) }()
	for key, b := range backends {
		launch(func() error { return b.Run(backendCtx, func(e core.Event) { rt.Event(backendCtx, key, e) }) })
	}
	for name, f := range frontends {
		launch(func() error { return f.Run(workerCtx, rt.Receiver(name)) })
	}
	for i, listener := range listeners {
		server := servers[i]
		launch(func() error {
			err := server.Serve(listener)
			if err == http.ErrServerClosed {
				return nil
			}
			return err
		})
	}
	for _, p := range c.Plugins {
		if p.Type == "ticker" {
			launch(func() error { plugins.Ticker(workerCtx, p.Interval, p.Route, p.Text, rt.Inject); return nil })
		}
	}
	launch(func() error {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			if err := store.Cleanup(time.Now(), c.Media.TTL); err != nil {
				slog.Error("media cleanup failed", "error", err)
			}
			select {
			case <-workerCtx.Done():
				return nil
			case <-tick.C:
			}
		}
	})
	slog.Info("router started", "api", c.API.Listen, "mcp", c.MCP.Listen, "frontends", len(frontends), "webhooks", len(webhooks))
	select {
	case <-sigCtx.Done():
	case err = <-errs:
	}
	lifecycle("Server shutting down ᗜ⩊ᗜ\n\nPhiRouter is going offline. You can send messages again after it restarts.")
	cancelWorkers()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if e := s.Shutdown(shutdown); e != nil {
			_ = s.Close()
		}
	}
	cancelRouter()
	<-routerDone
	cancelBackends()
	wg.Wait()
	return err
}
func notifyLifecycle(f core.Frontend, address, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.Send(ctx, address, core.Envelope{System: true, Parts: []core.Part{{Type: core.PartText, Text: text}}}); err != nil {
		slog.Error("lifecycle notification failed", "error", err)
	}
}
