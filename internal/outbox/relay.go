// Package outbox implements the transactional outbox relay (SDD section 7.1).
//
// Services write domain changes and outbox rows in a single PostgreSQL
// transaction. The Relay polls unpublished rows with SELECT ... FOR UPDATE
// SKIP LOCKED, publishes them to Kafka with acks=all, and stamps
// published_at. If the process crashes between publishing to Kafka and
// stamping published_at, the next poll republishes the message; downstream
// idempotency (engine dedupe window and processed_events table) makes the
// duplicate harmless.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/obs"
)

// Message is a single outbox row to be published.
type Message struct {
	ID      int64
	Topic   string
	Key     []byte
	Payload []byte
	Headers map[string]string
}

// Publisher abstracts synchronous batch publishing to Kafka.
type Publisher interface {
	Publish(ctx context.Context, msgs []Message) error
}

// FranzPublisher adapts a *kgo.Client to Publisher.
type FranzPublisher struct {
	Client *kgo.Client
}

// Publish sends all messages synchronously and returns the first error, if any.
func (p FranzPublisher) Publish(ctx context.Context, msgs []Message) error {
	records := make([]*kgo.Record, len(msgs))
	for i, m := range msgs {
		hdrs := make([]kgo.RecordHeader, 0, len(m.Headers))
		for k, v := range m.Headers {
			hdrs = append(hdrs, kgo.RecordHeader{Key: k, Value: []byte(v)})
		}
		records[i] = &kgo.Record{
			Topic:   m.Topic,
			Key:     m.Key,
			Value:   m.Payload,
			Headers: hdrs,
		}
	}
	return p.Client.ProduceSync(ctx, records...).FirstErr()
}

// InsertTx inserts an outbox row inside an existing pgx.Tx.
func InsertTx(ctx context.Context, tx pgx.Tx, aggregateID, topic string, key, payload []byte, headers map[string]string) error {
	if headers == nil {
		headers = map[string]string{}
	}
	hdrJSON, err := json.Marshal(headers)
	if err != nil {
		return fmt.Errorf("marshal outbox headers: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox (aggregate_id, topic, key, payload, headers)
		VALUES ($1, $2, $3, $4, $5)
	`, aggregateID, topic, key, payload, hdrJSON)
	return err
}

// Relay polls the outbox table and publishes unpublished messages.
type Relay struct {
	Pool         *pgxpool.Pool
	Pub          Publisher
	Log          *slog.Logger
	BatchSize    int
	PollInterval time.Duration
	wake         chan struct{}
}

// NewRelay constructs a Relay with sensible defaults.
func NewRelay(pool *pgxpool.Pool, pub Publisher, log *slog.Logger) *Relay {
	return &Relay{
		Pool:         pool,
		Pub:          pub,
		Log:          log,
		BatchSize:    256,
		PollInterval: 25 * time.Millisecond,
		wake:         make(chan struct{}, 1),
	}
}

// Wake nudges the relay to poll immediately without waiting for PollInterval.
func (r *Relay) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run loops until ctx is cancelled, draining unpublished outbox rows.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()

	for {
		for {
			n, err := r.DrainBatch(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				r.Log.Warn("outbox drain failed, backing off", "err", err)
				break
			}
			if n < r.BatchSize {
				break
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.wake:
		case <-ticker.C:
		}
	}
}

// DrainBatch locks up to BatchSize unpublished rows, publishes them, and marks
// them published_at = now() within the same transaction.
func (r *Relay) DrainBatch(ctx context.Context) (int, error) {
	tx, err := r.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, topic, key, payload, headers
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY id
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, r.BatchSize)
	if err != nil {
		return 0, err
	}
	var msgs []Message
	var ids []int64
	for rows.Next() {
		var m Message
		var hdrBytes []byte
		if err := rows.Scan(&m.ID, &m.Topic, &m.Key, &m.Payload, &hdrBytes); err != nil {
			rows.Close()
			return 0, err
		}
		if len(hdrBytes) > 0 {
			_ = json.Unmarshal(hdrBytes, &m.Headers)
		}
		msgs = append(msgs, m)
		ids = append(ids, m.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}

	if err := r.Pub.Publish(ctx, msgs); err != nil {
		return 0, fmt.Errorf("publish batch: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE outbox
		SET published_at = now()
		WHERE id = ANY($1)
	`, ids)
	if err != nil {
		return 0, fmt.Errorf("mark published: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	obs.OutboxBacklog.Set(float64(len(msgs)))
	return len(msgs), nil
}
