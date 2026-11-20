package main

import (
	"PhiRouter/adapters/backend/pi"
	"PhiRouter/adapters/frontend/telegram"
	"PhiRouter/internal/api"
	"PhiRouter/internal/config"
	"PhiRouter/internal/core"
	"PhiRouter/internal/mcp"
	"PhiRouter/internal/media"
	"PhiRouter/internal/router"
	"PhiRouter/plugins"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
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
	c, err := config.Load(*path, *dotenv)
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
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	agentDir := filepath.Join(home, ".pi", "agent")
	if err = os.MkdirAll(agentDir, 0700); err != nil {
		return err
	}
	backends := map[string]core.Backend{}
	for name, b := range c.Backends {
		if strings.ContainsAny(name, "/\\.") {
			return fmt.Errorf("backend name must not contain path separators or dots")
		}
		route := b.Type + ":" + name
		backends[route] = pi.New(pi.Options{Command: b.Command, Env: []string{"ROUTER_MCP__TOKEN=" + c.MCP.Token, "PHIROUTER_BACKEND=" + route}, StatePath: filepath.Join(agentDir, "phirouter-"+name+".session"), SessionFile: b.SessionFile, Attachment: c.Prompts.Attachment, InlineMax: c.Media.InlineImageMaxMB * 1024 * 1024, Media: store})
	}
	if err = registerMCP(filepath.Join(agentDir, "mcp.json"), "http://"+c.MCP.Listen+"/mcp"); err != nil {
		return err
	}
	tg := telegram.New(telegram.Options{Token: c.Telegram.Token, UserID: c.Telegram.UserID, Media: store, VoiceMaxSeconds: c.Media.VoiceMaxSeconds})
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
	rt := router.New(router.Options{Routes: routes, DefaultSource: "telegram:" + strconv.FormatInt(c.Telegram.UserID, 10), Separators: c.Stream.Separators, ShowTools: c.Stream.ShowToolCalls, QueueLimit: c.QueueLimit, Hooks: hooks}, map[string]core.Frontend{"telegram": tg}, backends)
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
	mcpRoutes := map[string]bool{}
	for key := range backends {
		mcpRoutes[key] = true
	}
	servers := []*http.Server{{Handler: api.New(c.API.Token, store.Validate, rt.Inject), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second}, {Handler: mcp.New(mcp.Options{Token: c.MCP.Token, Routes: mcpRoutes, Media: store, Send: rt.Send}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second}}
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
	routerDone := make(chan struct{})
	go func() { defer close(routerDone); _ = rt.Run(routerCtx) }()
	for key, b := range backends {
		launch(func() error { return b.Run(backendCtx, func(e core.Event) { rt.Event(backendCtx, key, e) }) })
	}
	launch(func() error { return tg.Run(workerCtx, rt.Inbound) })
	for i, listener := range []net.Listener{apiListener, mcpListener} {
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
	slog.Info("router started", "api", c.API.Listen, "mcp", c.MCP.Listen)
	select {
	case <-sigCtx.Done():
	case err = <-errs:
	}
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
func registerMCP(path, endpoint string) error {
	config := map[string]any{}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(raw, &config); err != nil {
			return fmt.Errorf("invalid existing mcp.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if config == nil {
		return fmt.Errorf("mcp.json must contain an object")
	}
	servers, ok := config["mcpServers"].(map[string]any)
	if !ok && config["mcpServers"] != nil {
		return fmt.Errorf("mcpServers must be an object")
	}
	if servers == nil {
		servers = map[string]any{}
	}
	servers["phirouter"] = map[string]any{"type": "http", "url": endpoint, "headers": map[string]string{"Authorization": "Bearer ${ROUTER_MCP__TOKEN}", "X-PhiRouter-Route": "${PHIROUTER_BACKEND}"}}
	config["mcpServers"] = servers
	raw, err = json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".phirouter.tmp"
	if err = os.WriteFile(tmp, append(raw, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
