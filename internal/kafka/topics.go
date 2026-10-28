package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
)

// Topic names (SDD section 4.1).
const (
	TopicOrderCommands = "orders.commands"
	TopicOrderEvents   = "orders.events"
	TopicTrades        = "trades.executed"
	TopicTicks         = "market.ticks"
	TopicCandles1m     = "market.candles.1m"
	TopicLedgerEvents  = "ledger.events"
)

// DLQ returns the dead-letter topic for a source topic.
func DLQ(topic string) string { return topic + ".dlq" }

// TopicSpec describes how a topic is created.
type TopicSpec struct {
	Name       string
	Partitions int32
	Configs    map[string]*string
}

func ptr(s string) *string { return &s }

func retention(d time.Duration) string { return fmt.Sprint(d.Milliseconds()) }

const day = 24 * time.Hour

// Topics returns the full topic catalogue including DLQs.
func Topics() []TopicSpec {
	base := []TopicSpec{
		{TopicOrderCommands, 6, map[string]*string{"retention.ms": ptr(retention(7 * day))}},
		{TopicOrderEvents, 6, map[string]*string{"retention.ms": ptr(retention(7 * day))}},
		{TopicTrades, 6, map[string]*string{"retention.ms": ptr(retention(30 * day))}},
		{TopicTicks, 3, map[string]*string{"retention.ms": ptr(retention(time.Hour))}},
		{TopicCandles1m, 3, map[string]*string{
			"cleanup.policy": ptr("compact,delete"),
			"retention.ms":   ptr(retention(7 * day)),
		}},
		{TopicLedgerEvents, 6, map[string]*string{"retention.ms": ptr(retention(7 * day))}},
	}
	out := make([]TopicSpec, 0, len(base)*2)
	out = append(out, base...)
	for _, t := range base {
		if t.Name == TopicTicks || t.Name == TopicCandles1m {
			continue // disposable / derived data: no DLQ needed
		}
		out = append(out, TopicSpec{DLQ(t.Name), 1, map[string]*string{"retention.ms": ptr(retention(14 * day))}})
	}
	return out
}

// EnsureTopics creates any missing topics. Existing topics are left as is.
func EnsureTopics(ctx context.Context, adm *kadm.Client, replication int16) error {
	var errs []error
	for _, t := range Topics() {
		resp, err := adm.CreateTopic(ctx, t.Partitions, replication, t.Configs, t.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
			continue
		}
		if resp.Err != nil && !errors.Is(resp.Err, kerr.TopicAlreadyExists) {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name, resp.Err))
		}
	}
	return errors.Join(errs...)
}
