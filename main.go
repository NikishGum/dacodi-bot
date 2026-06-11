package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"supportbot/auth"
	"supportbot/bot"
	"supportbot/config"
	"supportbot/poller"
	"supportbot/queue"
	store "supportbot/redis"
	"supportbot/registry"
	"supportbot/webhook"
	"supportbot/zoho"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// Logger not yet configured; use the default before exiting.
		slog.Error("loading configuration failed", "error", err)
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.SlogLevel()}))
	slog.SetDefault(logger)
	logger.Info("starting support bot", "telegram_mode", cfg.TelegramMode)

	// Root context cancelled on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			logger.Error("closing redis failed", "error", cerr)
		}
	}()

	api, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		return err
	}
	logger.Info("authenticated with telegram", "bot_username", api.Self.UserName)

	tokens := zoho.NewTokenManager(cfg.ZohoAccountsURL, cfg.ZohoClientID, cfg.ZohoClientSecret, cfg.ZohoRefreshToken, st, logger)
	deskClient := zoho.NewClient(cfg.ZohoDeskURL, cfg.ZohoOrgID, tokens, logger)
	desk := zoho.NewDesk(deskClient, zoho.DeskConfig{
		DeptID:        cfg.ZohoDeptID,
		CFChatID:      cfg.ZohoCFChatID,
		CFBotClientID: cfg.ZohoCFBotClientID,
		CFServices:    cfg.ZohoCFServices,
		CFPriority:    cfg.ZohoCFPriority,
		CFRequestID:   cfg.ZohoCFRequestID,
		ContactDomain: cfg.ZohoContactDomain,
	}, logger)
	if cfg.ZohoCFRequestID == "" {
		// FIX-5: without a request-id custom field the idempotency key is generated
		// but not stored, so duplicate detection cannot run. This is the graceful
		// degraded mode (today's behavior); warn so it is a deliberate choice.
		logger.Warn("ZOHO_CF_REQUEST_ID is not set: ticket idempotency tagging is disabled")
	}

	worker := queue.NewWorker(st, desk, api, logger)
	go worker.Run(ctx)

	reg := registry.New(st, desk, cfg, logger)
	tokenMgr := auth.NewManager(cfg.BindingTokenSecret, cfg.BindingTokenTTL, api.Self.UserName)

	b := bot.New(api, cfg, st, desk, worker, reg, tokenMgr, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler(st))

	// The Telegram webhook endpoint only exists in webhook mode. In polling mode
	// updates arrive over getUpdates, so leaving the route mounted would expose an
	// unauthenticated injection point (the webhook secret is not required outside
	// webhook mode) — see FIX-2.
	if cfg.TelegramMode == "webhook" {
		mux.HandleFunc("/telegram/webhook", b.WebhookHandler())
	}

	// Operator replies and ticket closures reach the bot either by Zoho pushing
	// to /zoho/webhook, or — when Zoho webhooks are unavailable — by the poller
	// pulling from Zoho on a timer. Exactly one path is active.
	switch cfg.ZohoSyncMode {
	case "poll":
		p := poller.New(st, desk, api, cfg.ZohoPollInterval, logger)
		go p.Run(ctx)
	case "webhook":
		zohoHandler := webhook.NewZohoHandler(api, st, cfg.ZohoWebhookToken, logger)
		mux.Handle("/zoho/webhook", zohoHandler)
	}

	srv := &http.Server{
		Addr:              cfg.ServerAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	switch cfg.TelegramMode {
	case "webhook":
		if err := b.ConfigureWebhook(ctx); err != nil {
			return err
		}
	case "polling":
		go b.StartPolling(ctx)
	}

	// Run the HTTP server until the root context is cancelled.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.ServerAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		logger.Error("http server error", "error", err)
		stop()
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		return err
	}
	logger.Info("shutdown complete")
	return nil
}

func healthHandler(st *store.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			http.Error(w, "redis unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
