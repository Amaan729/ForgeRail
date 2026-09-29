// Command forgerail runs the ForgeRail API server.
//
// With no configuration it starts in "everything in memory" mode: memory
// store, fake chain, in-process settlement. Point it at Postgres, Temporal
// and Base with the flags/env vars in config.go.
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

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/Amaan729/ForgeRail/internal/chain"
	"github.com/Amaan729/ForgeRail/internal/grpcapi"
	"github.com/Amaan729/ForgeRail/internal/httpapi"
	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/pgstore"
	"github.com/Amaan729/ForgeRail/internal/service"
	"github.com/Amaan729/ForgeRail/internal/settlement"
)

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Error("forgerail exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config, log *slog.Logger) error {
	// storage
	var store ledger.Store
	if cfg.DatabaseURL == "" {
		log.Warn("DATABASE_URL not set, using the in-memory store (data is lost on exit)")
		store = ledger.NewMemStore()
	} else {
		pool, err := pgstore.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		defer pool.Close()
		if err := pgstore.Migrate(ctx, pool); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		store = pgstore.New(pool)
	}

	// chain
	var ch chain.Client
	switch cfg.Chain {
	case "base":
		b, err := chain.DialBase(ctx, chain.BaseConfig{
			RPCURL:        cfg.BaseRPCURL,
			ChainID:       cfg.BaseChainID,
			TokenAddress:  cfg.USDCAddress,
			PrivateKeyHex: cfg.HotWalletKey,
			Confirmations: cfg.Confirmations,
		})
		if err != nil {
			return err
		}
		defer b.Close()
		log.Info("sending USDC on Base", "chain_id", cfg.BaseChainID, "hot_wallet", b.Address())
		ch = b
	default:
		log.Warn("using the fake chain, nothing is sent on-chain",
			"broadcast_fail_rate", cfg.FakeFailRate, "revert_rate", cfg.FakeRevertRate)
		ch = chain.NewFake(chain.FakeConfig{
			BroadcastFailRate: cfg.FakeFailRate,
			RevertRate:        cfg.FakeRevertRate,
			ConfirmAfter:      cfg.FakeConfirm,
			Seed:              uint64(time.Now().UnixNano()),
		})
	}
	acts := &settlement.Activities{Store: store, Chain: ch}
	if cfg.Chain == "fake" {
		// the fake chain confirms in milliseconds, no point polling every 2s
		acts.PollInterval = 100 * time.Millisecond
	}

	// settlement
	var starter settlement.Starter
	if cfg.TemporalAddr == "" {
		log.Warn("FORGERAIL_TEMPORAL_ADDR not set, settling withdrawals in-process (not durable)")
		runner := &settlement.LocalRunner{Acts: acts, Log: log}
		defer runner.Wait()
		starter = runner
	} else {
		tc, err := client.Dial(client.Options{
			HostPort:  cfg.TemporalAddr,
			Namespace: cfg.TemporalNamespace,
			Logger:    tlog.NewStructuredLogger(log),
		})
		if err != nil {
			return fmt.Errorf("temporal: %w", err)
		}
		defer tc.Close()
		if cfg.RunWorker {
			w := settlement.NewWorker(tc, acts)
			if err := w.Start(); err != nil {
				return fmt.Errorf("temporal worker: %w", err)
			}
			defer w.Stop()
		}
		starter = settlement.TemporalStarter{Client: tc}
	}

	svc := &service.Service{Store: store, Starter: starter, Log: log, AllowSystemTransfers: cfg.Dev}
	if cfg.Dev {
		log.Warn("dev mode: transfers out of system accounts are allowed")
	}

	// gRPC
	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return err
	}
	g := grpc.NewServer(grpc.UnaryInterceptor(grpcapi.LoggingInterceptor(log)))
	grpcapi.Register(g, svc)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(g, hs)
	reflection.Register(g)

	errc := make(chan error, 2)
	go func() {
		log.Info("gRPC listening", "addr", lis.Addr().String())
		errc <- g.Serve(lis)
	}()

	// REST
	var hsrv *http.Server
	if cfg.HTTPAddr != "" {
		var handler http.Handler = httpapi.New(svc, log)
		if cfg.ChaosErrRate > 0 || cfg.ChaosLostRate > 0 {
			log.Warn("CHAOS MODE: randomly failing and dropping POST responses",
				"err_rate", cfg.ChaosErrRate, "lost_rate", cfg.ChaosLostRate)
			handler = httpapi.Chaos(handler, cfg.ChaosErrRate, cfg.ChaosLostRate)
		}
		hsrv = &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
		}
		go func() {
			log.Info("REST listening", "addr", cfg.HTTPAddr)
			if err := hsrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return err
		}
	}
	hs.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if hsrv != nil {
		hsrv.Shutdown(shutdownCtx)
	}
	stopped := make(chan struct{})
	go func() { g.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		g.Stop()
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
