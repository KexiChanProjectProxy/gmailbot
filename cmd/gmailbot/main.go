// Command gmailbot is a self-hosted Telegram bot that delivers Gmail to Telegram.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kexiChanProjectProxy/gmailbot/internal/config"
	"github.com/kexiChanProjectProxy/gmailbot/internal/crypto"
	"github.com/kexiChanProjectProxy/gmailbot/internal/gmailx"
	"github.com/kexiChanProjectProxy/gmailbot/internal/oauthweb"
	"github.com/kexiChanProjectProxy/gmailbot/internal/store"
	"github.com/kexiChanProjectProxy/gmailbot/internal/syncer"
	"github.com/kexiChanProjectProxy/gmailbot/internal/telegram"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", os.Getenv("GMAILBOT_CONFIG"), "path to config.yaml (optional; env vars GMAILBOT_* override)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := run(*cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "gmailbot:", err)
		os.Exit(1)
	}
}

func newLogger(c config.Log) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.EqualFold(c.Format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log)
	log.Info("starting gmailbot", "version", version, "mode", cfg.Sync.Mode)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	key, _ := cfg.EncryptionKeyBytes()
	sealer, err := crypto.New(key)
	if err != nil {
		return err
	}
	if !sealer.Enabled() {
		log.Warn("db.encryption_key is not set; OAuth tokens are stored unencrypted")
	}

	st, err := store.Open(ctx, cfg.DB.Path)
	if err != nil {
		return err
	}
	defer st.Close()

	oauthCfg := gmailx.OAuthConfig(cfg.Google.ClientID, cfg.Google.ClientSecret, cfg.RedirectURL())
	gm := &gmailx.Factory{OAuth: oauthCfg, Store: st, Sealer: sealer}

	app, err := telegram.New(cfg, st, gm, log.With("component", "telegram"))
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	sy := &syncer.Syncer{
		Store: st, Gmail: gm, Notifier: app, Log: log.With("component", "sync"),
		Opts: syncer.Options{Dedup: cfg.Sync.Dedup},
	}

	web := &oauthweb.Server{OAuth: oauthCfg, Store: st, Gmail: gm, OnLinked: app.OnLinked, Log: log.With("component", "oauth")}

	var renewer *syncer.WatchRenewer
	if cfg.Sync.Mode == "pubsub" {
		renewer = &syncer.WatchRenewer{Syncer: sy, Topic: cfg.PubSub.Topic}
		app.Watcher = renewer // set before any goroutine reads it
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return app.Run(ctx) })
	g.Go(func() error { return web.Run(ctx, cfg.OAuth.ListenAddr) })
	g.Go(func() error { return (&syncer.Janitor{Store: st, TTL: cfg.Sync.CacheTTL, Log: log}).Run(ctx) })

	poll := cfg.Sync.PollInterval
	if renewer != nil {
		consumer := &syncer.PushConsumer{
			Syncer: sy, ProjectID: cfg.PubSub.ProjectID, Subscription: cfg.PubSub.Subscription,
			Debounce: &syncer.Debouncer{Syncer: sy, Delay: 2 * time.Second},
		}
		g.Go(func() error { return renewer.Run(ctx) })
		g.Go(func() error { return consumer.Run(ctx) })
		poll = cfg.Sync.FallbackPollInterval // Google recommends a slow fallback poll
	}
	g.Go(func() error {
		return (&syncer.Poller{Syncer: sy, Interval: poll, Workers: cfg.Sync.Workers}).Run(ctx)
	})

	err = g.Wait()
	log.Info("stopped")
	return err
}
