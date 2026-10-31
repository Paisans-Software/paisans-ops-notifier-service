// Command paisans-ops-notifier-service keeps each operator's Beszel alerts
// reaching them in a direct Matrix room. `serve`, the default, runs the
// service; `healthcheck` probes a running one, for the image's HEALTHCHECK.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/paisans-software/paisans-ops-notifier-service/internal/beszel"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/bot"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/config"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/mas"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/matrix"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/reconcile"
	"github.com/paisans-software/paisans-ops-notifier-service/internal/state"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "healthcheck":
		err = healthcheck()
	default:
		err = fmt.Errorf("unknown command %q: serve or healthcheck", cmd)
	}
	if err != nil {
		slog.Error("exiting", "err", err.Error())
		os.Exit(1)
	}
}

func healthcheck() error {
	addr := os.Getenv("OPS_NOTIFIER_LISTEN_ADDR")
	if addr == "" {
		addr = ":8085"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("OPS_NOTIFIER_LISTEN_ADDR is not host:port")
	}
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return errors.New("healthz did not answer")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %d", resp.StatusCode)
	}
	return nil
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := state.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: health(st), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("healthz server stopped", "err", err.Error())
			stop()
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	m := matrix.New(cfg.MatrixURL, nil, matrix.DefaultPacing)
	a := mas.New(cfg.MASAdminURL, cfg.MASClientID, cfg.MASClientSecret, nil)
	hub := beszel.New(cfg.BeszelURL, cfg.BeszelEmail, cfg.BeszelPassword, nil)

	// MAS or the homeserver may still be starting: wait for the bot, as
	// long as it takes, unless the problem is one only a configuration
	// change ends.
	var b *bot.Bot
	for wait := time.Second; ; wait = min(wait*2, 5*time.Minute) {
		b, err = bot.Start(ctx, m, a, st, bot.Config{ServerName: cfg.ServerName, Localpart: cfg.BotLocalpart, DisplayName: cfg.BotDisplayName})
		if err == nil {
			break
		}
		if errors.Is(err, bot.ErrOff) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("the bot could not start; retrying", "reason", startReason(err), "retry_in", wait.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
	slog.Info("started", "bot", b.UserID())

	// The hub may still be starting, or the service may have been stopped
	// for longer than a token lives. Nothing runs until the hub takes a
	// fresh superuser sign in: a token it no longer takes reads as a
	// guest's, whose list of users is empty.
	for wait := time.Second; ; wait = min(wait*2, 5*time.Minute) {
		err := hub.EnsureAuth(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("the Beszel hub could not be signed in to; retrying", "reason", hubReason(err), "retry_in", wait.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}

	n := reconcile.New(reconcile.Config{Host: cfg.MatrixHost, ServerName: cfg.ServerName, Provider: cfg.MASProvider,
		Operators: cfg.Operators, Keep: cfg.KeepEmails}, b, a, hub, st)
	go func() { _ = n.Listen(ctx) }()

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		if err := n.Pass(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("a pass did not finish; the next one will", "errors", countErrors(err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// health answers /healthz with ok while the state file is readable.
func health(st *state.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !st.Readable() {
			http.Error(w, "state unreadable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func startReason(err error) string {
	if errors.Is(err, bot.ErrUnavailable) {
		return "homeserver or MAS unavailable"
	}
	var me *matrix.Error
	if errors.As(err, &me) {
		return me.Error()
	}
	return "internal error"
}

func hubReason(err error) string {
	if errors.Is(err, beszel.ErrUnavailable) {
		return "beszel hub unavailable"
	}
	var se *beszel.StatusError
	if errors.As(err, &se) {
		return se.Error()
	}
	return "internal error"
}

// countErrors is how many errors a joined error holds.
func countErrors(err error) int {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return len(j.Unwrap())
	}
	return 1
}
