// Package testpg spins up an ephemeral PostgreSQL database (using either
// $TEST_DATABASE_URL or a local user-space pg_ctl cluster over a Unix socket in
// /tmp) and applies all OrderFlow migrations.
package testpg

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tejas004s/orderflow/migrations"
)

var (
	clusterOnce sync.Once
	clusterSock string
	clusterErr  error
)

func findPgBin(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	matches, _ := filepath.Glob("/usr/lib/postgresql/*/bin/" + name)
	if len(matches) > 0 {
		return matches[len(matches)-1]
	}
	return ""
}

func pingUnix(sockDir string) bool {
	u := fmt.Sprintf("postgres://postgres@/postgres?host=%s&sslmode=disable", url.QueryEscape(sockDir))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	p, err := pgxpool.New(ctx, u)
	if err != nil {
		return false
	}
	defer p.Close()
	return p.Ping(ctx) == nil
}

func ensureCluster() (string, string, error) {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u, "", nil
	}
	clusterOnce.Do(func() {
		dir := "/tmp/orderflow-testpg-shared"
		sockDir := filepath.Join(dir, "sock")
		if pingUnix(sockDir) {
			clusterSock = sockDir
			return
		}
		initdb := findPgBin("initdb")
		pgctl := findPgBin("pg_ctl")
		if initdb == "" || pgctl == "" {
			clusterErr = fmt.Errorf("initdb/pg_ctl not found and TEST_DATABASE_URL not set")
			return
		}
		_ = os.RemoveAll(dir)
		dataDir := filepath.Join(dir, "data")
		if err := os.MkdirAll(sockDir, 0o755); err != nil {
			clusterErr = err
			return
		}

		cmd := exec.Command(initdb, "-D", dataDir, "-U", "postgres", "-A", "trust", "--no-sync", "--encoding=UTF8", "--locale=C")
		if out, err := cmd.CombinedOutput(); err != nil {
			clusterErr = fmt.Errorf("initdb: %v: %s", err, out)
			return
		}

		opts := fmt.Sprintf("-F -c listen_addresses= -p 5432 -k %s", sockDir)
		start := exec.Command(pgctl, "-D", dataDir, "-l", filepath.Join(dir, "pg.log"), "-o", opts, "-w", "start")
		if out, err := start.CombinedOutput(); err != nil {
			logBytes, _ := os.ReadFile(filepath.Join(dir, "pg.log"))
			clusterErr = fmt.Errorf("pg_ctl start: %v: %s (%s)", err, out, logBytes)
			return
		}
		clusterSock = sockDir
	})
	if clusterErr != nil {
		return "", "", clusterErr
	}
	u := fmt.Sprintf("postgres://postgres@/postgres?host=%s&sslmode=disable", url.QueryEscape(clusterSock))
	return u, clusterSock, nil
}

// New creates a fresh, migrated PostgreSQL database for t and returns its
// connection pool and URL.
func New(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	adminURL, sockDir, err := ensureCluster()
	if err != nil {
		t.Skipf("skipping postgres test: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer adminPool.Close()

	dbName := "of_" + uuid.NewString()[:8]
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create db %s: %v", dbName, err)
	}

	var dbURL string
	if sockDir != "" {
		dbURL = fmt.Sprintf("postgres://postgres@/%s?host=%s&sslmode=disable", dbName, url.QueryEscape(sockDir))
	} else {
		cfg, err := pgxpool.ParseConfig(adminURL)
		if err != nil {
			t.Fatalf("parse admin url: %v", err)
		}
		dbURL = fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable",
			cfg.ConnConfig.User, cfg.ConnConfig.Host, cfg.ConnConfig.Port, dbName)
	}

	if err := migrations.Up(dbURL); err != nil {
		t.Fatalf("migrate %s: %v", dbName, err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		pool.Close()
	})
	return pool, dbURL
}
