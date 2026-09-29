package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/Amaan729/ForgeRail/internal/chain"
)

// config comes from flags, and every flag can also be set with an env var
// (flag wins). See README for the full list.
type config struct {
	GRPCAddr string
	HTTPAddr string
	LogLevel string

	DatabaseURL string // empty = in-memory store

	TemporalAddr      string // empty = in-process LocalRunner
	TemporalNamespace string
	RunWorker         bool

	Chain string // fake | base

	BaseRPCURL     string
	BaseChainID    int64
	USDCAddress    string
	HotWalletKey   string
	Confirmations  uint64
	FakeFailRate   float64
	FakeRevertRate float64
	FakeConfirm    time.Duration

	Dev bool

	ChaosErrRate  float64
	ChaosLostRate float64
}

func loadConfig(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("forgerail", flag.ContinueOnError)
	fs.StringVar(&c.GRPCAddr, "grpc-addr", env("FORGERAIL_GRPC_ADDR", ":9090"), "gRPC listen address")
	fs.StringVar(&c.HTTPAddr, "http-addr", env("FORGERAIL_HTTP_ADDR", ":8080"), "REST listen address (empty to disable)")
	fs.StringVar(&c.LogLevel, "log-level", env("FORGERAIL_LOG_LEVEL", "info"), "debug|info|warn|error")
	fs.StringVar(&c.DatabaseURL, "database-url", env("DATABASE_URL", ""), "Postgres URL; empty uses an in-memory store")
	fs.StringVar(&c.TemporalAddr, "temporal-addr", env("FORGERAIL_TEMPORAL_ADDR", ""), "Temporal frontend host:port; empty runs settlement in-process")
	fs.StringVar(&c.TemporalNamespace, "temporal-namespace", env("FORGERAIL_TEMPORAL_NAMESPACE", "default"), "Temporal namespace")
	fs.BoolVar(&c.RunWorker, "worker", envBool("FORGERAIL_WORKER", true), "also run the Temporal worker in this process")
	fs.StringVar(&c.Chain, "chain", env("FORGERAIL_CHAIN", "fake"), "fake|base")
	fs.StringVar(&c.BaseRPCURL, "base-rpc-url", env("FORGERAIL_BASE_RPC_URL", "https://sepolia.base.org"), "Base JSON-RPC endpoint")
	fs.Int64Var(&c.BaseChainID, "base-chain-id", envInt("FORGERAIL_BASE_CHAIN_ID", chain.ChainIDBaseSepolia), "expected chain id")
	fs.StringVar(&c.USDCAddress, "usdc-address", env("FORGERAIL_USDC_ADDRESS", chain.USDCBaseSepolia), "USDC token contract")
	fs.StringVar(&c.HotWalletKey, "hot-wallet-key", env("FORGERAIL_HOT_WALLET_KEY", ""), "hex private key of the sending wallet (testnet!)")
	fs.Uint64Var(&c.Confirmations, "confirmations", uint64(envInt("FORGERAIL_CONFIRMATIONS", 3)), "blocks before a withdrawal counts as settled")
	fs.Float64Var(&c.FakeFailRate, "fake-broadcast-fail-rate", envFloat("FORGERAIL_FAKE_BROADCAST_FAIL_RATE", 0), "fake chain: chance a broadcast errors")
	fs.Float64Var(&c.FakeRevertRate, "fake-revert-rate", envFloat("FORGERAIL_FAKE_REVERT_RATE", 0), "fake chain: chance a tx reverts")
	fs.DurationVar(&c.FakeConfirm, "fake-confirm-after", envDuration("FORGERAIL_FAKE_CONFIRM_AFTER", 500*time.Millisecond), "fake chain: time to confirmation")
	fs.BoolVar(&c.Dev, "dev", envBool("FORGERAIL_DEV", false), "dev mode: allow transfers out of system accounts (funding test accounts)")
	fs.Float64Var(&c.ChaosErrRate, "chaos-error-rate", envFloat("FORGERAIL_CHAOS_ERROR_RATE", 0), "load testing: chance a POST fails with 503 before it is handled")
	fs.Float64Var(&c.ChaosLostRate, "chaos-lost-response-rate", envFloat("FORGERAIL_CHAOS_LOST_RESPONSE_RATE", 0), "load testing: chance a POST is handled but the client gets a 503")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	switch c.Chain {
	case "fake":
	case "base":
		if c.HotWalletKey == "" {
			return c, fmt.Errorf("-chain=base needs -hot-wallet-key / FORGERAIL_HOT_WALLET_KEY")
		}
	default:
		return c, fmt.Errorf("unknown -chain %q", c.Chain)
	}
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
