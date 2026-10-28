// Command ofctl is the OrderFlow admin CLI.
//
//	ofctl migrate          apply database migrations ($DATABASE_URL)
//	ofctl topics           create Kafka topics ($KAFKA_BROKERS)
//	ofctl init             migrate + topics (used by docker compose)
//	ofctl health <url>     exit 0 if GET url returns 2xx (container healthchecks)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/config"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/migrations"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "migrate":
		err = migrate()
	case "topics":
		err = topics()
	case "init":
		if err = migrate(); err == nil {
			err = topics()
		}
	case "health":
		if len(os.Args) < 3 {
			usage()
		}
		err = health(os.Args[2])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ofctl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ofctl migrate|topics|init|health <url>")
	os.Exit(2)
}

func migrate() error {
	cfg := config.Load("ofctl", "")
	var err error
	for i := 0; i < 30; i++ { // tolerate Postgres still starting
		if err = migrations.Up(cfg.DatabaseURL); err == nil {
			fmt.Println("migrations applied")
			return nil
		}
		time.Sleep(time.Second)
	}
	return err
}

func topics() error {
	cfg := config.Load("ofctl", "")
	cl, err := kgo.NewClient(kgo.SeedBrokers(cfg.KafkaBrokers...))
	if err != nil {
		return err
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if err = cl.Ping(ctx); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("kafka unreachable: %w", err)
	}
	repl := int16(config.Int("KAFKA_REPLICATION", 1))
	if err := kafka.EnsureTopics(ctx, kadm.NewClient(cl), repl); err != nil {
		return err
	}
	fmt.Println("topics ensured")
	return nil
}

func health(url string) error {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	return nil
}
