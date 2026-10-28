// Package kafka wraps franz-go with OrderFlow's delivery defaults:
// idempotent producers with acks=all, and consumers with manual offset
// commits (SDD section 4.4).
package kafka

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"
)

// ProducerOpts returns client options for a durable, idempotent producer.
// franz-go enables idempotence by default; we make acks=all explicit.
func ProducerOpts(brokers []string, clientID string) []kgo.Opt {
	return []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ClientID(clientID),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerLinger(0),
		kgo.ProducerBatchCompression(kgo.Lz4Compression(), kgo.NoCompression()),
	}
}

// ConsumerOpts returns client options for a group consumer with manual
// commits. Offsets must be committed only after side effects succeed.
func ConsumerOpts(brokers []string, clientID, group string, topics ...string) []kgo.Opt {
	return []kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ClientID(clientID),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchIsolationLevel(kgo.ReadUncommitted()),
	}
}

// Check returns a readiness check that pings the cluster.
func Check(cl *kgo.Client) func(ctx context.Context) error {
	return func(ctx context.Context) error { return cl.Ping(ctx) }
}
