// Package app wires the shared dependencies (config, logger, Postgres,
// Kafka, service runtime) that every OrderFlow binary needs.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/config"
	"github.com/tejas004s/orderflow/internal/db"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/obs"
	"github.com/tejas004s/orderflow/internal/svc"
)

// Needs declares which infrastructure a binary depends on.
type Needs struct {
	DB         bool
	DBMaxConns int32
	Kafka      bool // an admin/ping client used for readiness
}

// Deps is the bundle returned by Bootstrap.
type Deps struct {
	Cfg   config.Config
	Log   *slog.Logger
	DB    *pgxpool.Pool
	Kafka *kgo.Client
	Svc   *svc.Service
}

// Bootstrap loads config, builds the logger and connects to the requested
// infrastructure, registering readiness checks and cleanup hooks.
func Bootstrap(name, defaultAddr string, n Needs) (*Deps, error) {
	cfg := config.Load(name, defaultAddr)
	log := obs.NewLogger(name, cfg.LogLevel)
	d := &Deps{Cfg: cfg, Log: log, Svc: svc.New(name, log)}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if n.DB {
		pool, err := db.Connect(ctx, cfg.DatabaseURL, n.DBMaxConns, log)
		if err != nil {
			return nil, err
		}
		d.DB = pool
		d.Svc.AddCheck("postgres", db.Check(pool))
		d.Svc.OnClose(pool.Close)
	}
	if n.Kafka {
		cl, err := kgo.NewClient(kgo.SeedBrokers(cfg.KafkaBrokers...), kgo.ClientID(name+"-admin"))
		if err != nil {
			return nil, fmt.Errorf("kafka client: %w", err)
		}
		d.Kafka = cl
		d.Svc.AddCheck("kafka", kafka.Check(cl))
		d.Svc.OnClose(cl.Close)
	}
	log.Info("bootstrapped", "env", cfg.Env, "http", cfg.HTTPAddr)
	return d, nil
}
