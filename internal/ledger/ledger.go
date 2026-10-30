// Package ledger settles trades and order lifecycle events into PostgreSQL
// with idempotent consumer semantics and a double-entry ledger
// (SDD sections 6.2, 7.2, 7.3, 7.4).
package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tejas004s/orderflow/internal/events"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/outbox"
)

// ConsumerName identifies ledger-service in the processed_events table.
const ConsumerName = "ledger-service"

// ExchangeFeesUserID is the system user that collects net exchange trading fees.
var ExchangeFeesUserID = uuid.MustParse("00000000-0000-0000-0000-00000000fee5")

// ExchangeFeesAccountRef is the double-entry ledger account_ref for exchange fee revenue.
const ExchangeFeesAccountRef = "system:exchange_fees"

// Default fee schedule in basis points (1 bps = 0.01%).
const (
	DefaultMakerFeeBps int64 = -1 // -1 bps maker rebate
	DefaultTakerFeeBps int64 = 5  // +5 bps taker fee
)

// Option configures a ledger Service.
type Option func(*Service)

// WithFees configures maker rebate (negative bps) / fee and taker fee (positive bps).
func WithFees(makerBps, takerBps int64) Option {
	return func(s *Service) {
		s.MakerFeeBps = makerBps
		s.TakerFeeBps = takerBps
	}
}

// Service applies trade and order events to PostgreSQL.
type Service struct {
	Pool        *pgxpool.Pool
	Pub         outbox.Publisher // optional: for DLQ routing
	Log         *slog.Logger
	MakerFeeBps int64
	TakerFeeBps int64
}

// NewService creates a ledger Service.
func NewService(pool *pgxpool.Pool, pub outbox.Publisher, log *slog.Logger, opts ...Option) *Service {
	s := &Service{Pool: pool, Pub: pub, Log: log}
	for _, o := range opts {
		o(s)
	}
	return s
}

// HandleRaw decodes an envelope from orders.events or trades.executed and
// applies it idempotently inside a single database transaction.
func (s *Service) HandleRaw(ctx context.Context, topic string, key, value []byte) error {
	env, err := events.Unwrap(value, nil)
	if err != nil {
		return s.sendDLQ(ctx, topic, key, value, err)
	}
	switch env.EventType {
	case events.TypeTradeExecuted:
		var tr events.TradeExecuted
		if _, err := events.Unwrap(value, &tr); err != nil {
			return s.sendDLQ(ctx, topic, key, value, err)
		}
		_, err := s.ApplyTrade(ctx, env.EventID, tr)
		return err
	case events.TypeOrderEvent:
		var oev events.OrderEvent
		if _, err := events.Unwrap(value, &oev); err != nil {
			return s.sendDLQ(ctx, topic, key, value, err)
		}
		_, err := s.ApplyOrderEvent(ctx, env.EventID, oev)
		return err
	default:
		return s.sendDLQ(ctx, topic, key, value, fmt.Errorf("unexpected event_type %q", env.EventType))
	}
}

// ApplyTrade settles a single trade atomically. It returns applied=false if
// eventID was already recorded in processed_events.
func (s *Service) ApplyTrade(ctx context.Context, eventID uuid.UUID, tr events.TradeExecuted) (bool, error) {
	if eventID == uuid.Nil {
		eventID = tr.MatchID
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Idempotency guard (SDD section 7.2).
	ok, err := markProcessedTx(ctx, tx, eventID)
	if err != nil || !ok {
		return false, err
	}

	// 2. Lock buyer and seller orders in deterministic UUID order (ADR-006).
	orderIDs := []uuid.UUID{tr.BuyOrderID, tr.SellOrderID}
	sortUUIDs(orderIDs)
	for _, oid := range orderIDs {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM orders WHERE id = $1 FOR UPDATE`, oid); err != nil {
			return false, err
		}
	}

	var buyUnitReserve int64
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(price, protection_price, 0)
		FROM orders WHERE id = $1
	`, tr.BuyOrderID).Scan(&buyUnitReserve)
	if err != nil {
		return false, fmt.Errorf("load buy order %s: %w", tr.BuyOrderID, err)
	}
	// Prefer the price the engine matched the buy order at over the stored
	// one: a replace of the order may or may not have been applied to the
	// stored row yet, whereas the engine's value is exactly what was reserved
	// for this slice of the order.
	if tr.BuyLimitPrice > 0 {
		buyUnitReserve = tr.BuyLimitPrice
	}

	// 3. Ensure buyer's position row exists before locking positions in user_id order.
	_, err = tx.Exec(ctx, `
		INSERT INTO positions (user_id, symbol, qty, reserved_qty, avg_cost)
		VALUES ($1, $2, 0, 0, 0)
		ON CONFLICT (user_id, symbol) DO NOTHING
	`, tr.BuyerID, tr.Symbol)
	if err != nil {
		return false, err
	}

	feesEnabled := s.TakerFeeBps != 0 || s.MakerFeeBps != 0
	if feesEnabled {
		_, _ = tx.Exec(ctx, `
			INSERT INTO users (id, email, password_hash, is_bot)
			VALUES ($1, 'exchange_fees@system.orderflow', 'system', true)
			ON CONFLICT (id) DO NOTHING
		`, ExchangeFeesUserID)
		_, _ = tx.Exec(ctx, `
			INSERT INTO accounts (user_id, currency, available, reserved)
			VALUES ($1, 'USD', 0, 0)
			ON CONFLICT (user_id, currency) DO NOTHING
		`, ExchangeFeesUserID)
	}

	userIDs := []uuid.UUID{tr.BuyerID, tr.SellerID}
	if feesEnabled {
		userIDs = append(userIDs, ExchangeFeesUserID)
	}
	sortUUIDs(userIDs)
	for _, uid := range userIDs {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM accounts WHERE user_id = $1 AND currency = 'USD' FOR UPDATE`, uid); err != nil {
			return false, err
		}
		if uid != ExchangeFeesUserID {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM positions WHERE user_id = $1 AND symbol = $2 FOR UPDATE`, uid, tr.Symbol); err != nil {
				return false, err
			}
		}
	}

	// 4. Insert immutable trade row.
	execTime := time.Unix(0, tr.ExecutedAt).UTC()
	if tr.ExecutedAt == 0 {
		execTime = time.Now().UTC()
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO trades (
			match_id, symbol, price, qty, buy_order_id, sell_order_id,
			buyer_id, seller_id, aggressor_side, executed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (match_id) DO NOTHING
	`, tr.MatchID, tr.Symbol, tr.Price, tr.Qty, tr.BuyOrderID, tr.SellOrderID,
		tr.BuyerID, tr.SellerID, tr.AggressorSide, execTime)
	if err != nil {
		return false, fmt.Errorf("insert trade: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Already settled under a different event envelope.
		return false, nil
	}

	// 5. Move cash: buyer spends tr.Price*tr.Qty from reserved; any price
	// improvement (buyUnitReserve - tr.Price)*tr.Qty returns to buyer.available.
	cost := tr.Price * tr.Qty
	reservedConsumed := buyUnitReserve * tr.Qty
	if reservedConsumed < cost {
		reservedConsumed = cost
	}
	priceImprovement := reservedConsumed - cost

	_, err = tx.Exec(ctx, `
		UPDATE accounts
		SET reserved  = reserved - $1,
		    available = available + $2
		WHERE user_id = $3 AND currency = 'USD'
	`, reservedConsumed, priceImprovement, tr.BuyerID)
	if err != nil {
		return false, fmt.Errorf("debit buyer cash: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE accounts
		SET available = available + $1
		WHERE user_id = $2 AND currency = 'USD'
	`, cost, tr.SellerID)
	if err != nil {
		return false, fmt.Errorf("credit seller cash: %w", err)
	}

	// 5b. Settle maker rebate / taker fee into the system EXCHANGE_FEES account.
	var makerFee, takerFee int64
	takerID, makerID := tr.BuyerID, tr.SellerID
	if tr.AggressorSide == "SELL" {
		takerID, makerID = tr.SellerID, tr.BuyerID
	}
	if feesEnabled {
		if s.TakerFeeBps > 0 {
			var takerAvail int64
			if err := tx.QueryRow(ctx, `SELECT available FROM accounts WHERE user_id = $1 AND currency = 'USD'`, takerID).Scan(&takerAvail); err != nil {
				return false, err
			}
			takerFee = min((cost*s.TakerFeeBps)/10000, takerAvail)
		}
		if s.MakerFeeBps < 0 {
			rebate := min((cost*(-s.MakerFeeBps))/10000, takerFee)
			makerFee = -rebate
		} else if s.MakerFeeBps > 0 {
			var makerAvail int64
			if err := tx.QueryRow(ctx, `SELECT available FROM accounts WHERE user_id = $1 AND currency = 'USD'`, makerID).Scan(&makerAvail); err != nil {
				return false, err
			}
			makerFee = min((cost*s.MakerFeeBps)/10000, makerAvail)
		}
		if takerFee > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE accounts SET available = available - $1 WHERE user_id = $2 AND currency = 'USD'
			`, takerFee, takerID); err != nil {
				return false, fmt.Errorf("debit taker fee: %w", err)
			}
		}
		if makerFee != 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE accounts SET available = available - $1 WHERE user_id = $2 AND currency = 'USD'
			`, makerFee, makerID); err != nil {
				return false, fmt.Errorf("apply maker fee/rebate: %w", err)
			}
		}
		netExchangeFee := takerFee + makerFee
		if netExchangeFee != 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE accounts SET available = available + $1 WHERE user_id = $2 AND currency = 'USD'
			`, netExchangeFee, ExchangeFeesUserID); err != nil {
				return false, fmt.Errorf("credit exchange fee account: %w", err)
			}
		}
		_, _ = tx.Exec(ctx, `
			UPDATE trades SET maker_fee = $1, taker_fee = $2 WHERE match_id = $3
		`, makerFee, takerFee, tr.MatchID)
	}

	// 6. Move shares: deduct from seller.reserved_qty, add to buyer.qty and
	// update weighted-average cost.
	_, err = tx.Exec(ctx, `
		UPDATE positions
		SET reserved_qty = reserved_qty - $1
		WHERE user_id = $2 AND symbol = $3
	`, tr.Qty, tr.SellerID, tr.Symbol)
	if err != nil {
		return false, fmt.Errorf("debit seller shares: %w", err)
	}

	var buyerQty, buyerReservedQty, buyerAvg int64
	err = tx.QueryRow(ctx, `
		SELECT qty, reserved_qty, avg_cost
		FROM positions
		WHERE user_id = $1 AND symbol = $2
	`, tr.BuyerID, tr.Symbol).Scan(&buyerQty, &buyerReservedQty, &buyerAvg)
	if err != nil {
		return false, err
	}
	existingTotalShares := buyerQty + buyerReservedQty
	newTotalShares := existingTotalShares + tr.Qty
	newAvgCost := (existingTotalShares*buyerAvg + cost) / newTotalShares

	_, err = tx.Exec(ctx, `
		UPDATE positions
		SET qty = qty + $1, avg_cost = $2
		WHERE user_id = $3 AND symbol = $4
	`, tr.Qty, newAvgCost, tr.BuyerID, tr.Symbol)
	if err != nil {
		return false, fmt.Errorf("credit buyer shares: %w", err)
	}

	// 7. Update buy and sell order filled_qty, reserved_amount, and status.
	_, err = tx.Exec(ctx, `
		UPDATE orders
		SET filled_qty      = filled_qty + $1,
		    reserved_amount = reserved_amount - $2,
		    status = CASE
		        WHEN filled_qty + $1 >= qty THEN 'FILLED'
		        WHEN status IN ('NEW', 'ACCEPTED') THEN 'PARTIALLY_FILLED'
		        ELSE status
		    END,
		    updated_at = now()
		WHERE id = $3
	`, tr.Qty, reservedConsumed, tr.BuyOrderID)
	if err != nil {
		return false, fmt.Errorf("update buy order: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE orders
		SET filled_qty      = filled_qty + $1,
		    reserved_amount = reserved_amount - $1,
		    status = CASE
		        WHEN filled_qty + $1 >= qty THEN 'FILLED'
		        WHEN status IN ('NEW', 'ACCEPTED') THEN 'PARTIALLY_FILLED'
		        ELSE status
		    END,
		    updated_at = now()
		WHERE id = $2
	`, tr.Qty, tr.SellOrderID)
	if err != nil {
		return false, fmt.Errorf("update sell order: %w", err)
	}

	// 8. Write double-entry ledger rows (SDD section 6.2).
	// For each (txn_id, asset), sum(DEBIT) == sum(CREDIT).
	buyerRef := "user:" + tr.BuyerID.String()
	sellerRef := "user:" + tr.SellerID.String()
	takerRef := "user:" + takerID.String()
	makerRef := "user:" + makerID.String()
	type ledgerRow struct {
		account   string
		asset     string
		direction string
		amount    int64
	}
	entries := []ledgerRow{
		{buyerRef, "USD", "CREDIT", cost},
		{sellerRef, "USD", "DEBIT", cost},
		{sellerRef, tr.Symbol, "CREDIT", tr.Qty},
		{buyerRef, tr.Symbol, "DEBIT", tr.Qty},
	}
	if takerFee > 0 {
		entries = append(entries,
			ledgerRow{takerRef, "USD", "CREDIT", takerFee},
			ledgerRow{ExchangeFeesAccountRef, "USD", "DEBIT", takerFee},
		)
	}
	if makerFee < 0 {
		rebate := -makerFee
		entries = append(entries,
			ledgerRow{ExchangeFeesAccountRef, "USD", "CREDIT", rebate},
			ledgerRow{makerRef, "USD", "DEBIT", rebate},
		)
	} else if makerFee > 0 {
		entries = append(entries,
			ledgerRow{makerRef, "USD", "CREDIT", makerFee},
			ledgerRow{ExchangeFeesAccountRef, "USD", "DEBIT", makerFee},
		)
	}
	for _, e := range entries {
		_, err = tx.Exec(ctx, `
			INSERT INTO ledger_entries (txn_id, account_ref, asset, direction, amount, match_id)
			VALUES ($1, $2, $3, $4, $5, $1)
		`, tr.MatchID, e.account, e.asset, e.direction, e.amount)
		if err != nil {
			return false, fmt.Errorf("insert ledger entry: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ApplyOrderEvent applies an OrderEvent idempotently. When an order transitions
// to CANCELLED or REJECTED, any reservation for the unfilled quantity
// (ev.Qty - ev.FilledQty) is released back to available cash or shares.
// REPLACED and REPLACE_REJECT events settle a cancel-replace (see replace.go).
func (s *Service) ApplyOrderEvent(ctx context.Context, eventID uuid.UUID, ev events.OrderEvent) (bool, error) {
	if eventID == uuid.Nil {
		eventID = ev.EventID
	}
	if ev.Kind == "DUPLICATE" || ev.Kind == "CANCEL_REJECT" || ev.Kind == "SYMBOL_HALTED" {
		return false, nil
	}

	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ok, err := markProcessedTx(ctx, tx, eventID)
	if err != nil || !ok {
		return false, err
	}

	var (
		userID         uuid.UUID
		symbol         string
		side           string
		unitReserve    int64
		orderQty       int64
		curStatus      string
		reservedAmount int64
	)
	err = tx.QueryRow(ctx, `
		SELECT user_id, symbol, side, COALESCE(price, protection_price, 0), qty, status, reserved_amount
		FROM orders
		WHERE id = $1
		FOR UPDATE
	`, ev.OrderID).Scan(&userID, &symbol, &side, &unitReserve, &orderQty, &curStatus, &reservedAmount)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// A refused replace: give back what order-service reserved for it and
	// restore the quantity it raised. The order itself is unchanged.
	if ev.Kind == "REPLACE_REJECT" {
		if err := settleReplaceReject(ctx, tx, ev, userID, symbol, side, reservedAmount); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return true, nil
	}

	// An accepted replace: adopt the new price/qty and true up the reservation.
	if ev.Kind == "REPLACED" {
		reservedAmount, err = settleReplaced(ctx, tx, ev, userID, symbol, side, reservedAmount)
		if err != nil {
			return false, err
		}
		unitReserve, orderQty = ev.Price, ev.Qty
	}

	// Release unfilled reservation on transition to CANCELLED or REJECTED.
	if (ev.Status == "CANCELLED" || ev.Status == "REJECTED") &&
		curStatus != "CANCELLED" && curStatus != "REJECTED" && curStatus != "FILLED" {

		unfilledQty := orderQty - ev.FilledQty
		if unfilledQty < 0 {
			unfilledQty = 0
		}
		if side == "BUY" {
			releaseCash := unitReserve * unfilledQty
			if releaseCash > reservedAmount {
				releaseCash = reservedAmount
			}
			if releaseCash > 0 {
				_, err = tx.Exec(ctx, `
					UPDATE accounts
					SET reserved  = reserved - $1,
					    available = available + $1
					WHERE user_id = $2 AND currency = 'USD'
				`, releaseCash, userID)
				if err != nil {
					return false, fmt.Errorf("release buyer cash: %w", err)
				}
				reservedAmount -= releaseCash
			}
		} else {
			releaseShares := unfilledQty
			if releaseShares > reservedAmount {
				releaseShares = reservedAmount
			}
			if releaseShares > 0 {
				_, err = tx.Exec(ctx, `
					UPDATE positions
					SET reserved_qty = reserved_qty - $1,
					    qty          = qty + $1
					WHERE user_id = $2 AND symbol = $3
				`, releaseShares, userID, symbol)
				if err != nil {
					return false, fmt.Errorf("release seller shares: %w", err)
				}
				reservedAmount -= releaseShares
			}
		}
	}

	newStatus := curStatus
	if statusRank(ev.Status) >= statusRank(curStatus) && !isTerminalStatus(curStatus) {
		newStatus = ev.Status
	}

	var rejectReason *string
	if ev.Reason != "" && (newStatus == "CANCELLED" || newStatus == "REJECTED") {
		r := ev.Reason
		rejectReason = &r
	}

	_, err = tx.Exec(ctx, `
		UPDATE orders
		SET status          = $1,
		    reserved_amount = $2,
		    reject_reason   = COALESCE($3, reject_reason),
		    updated_at      = now()
		WHERE id = $4
	`, newStatus, reservedAmount, rejectReason, ev.OrderID)
	if err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func statusRank(st string) int {
	switch st {
	case "NEW":
		return 0
	case "ACCEPTED":
		return 1
	case "PARTIALLY_FILLED":
		return 2
	case "FILLED", "CANCELLED", "REJECTED":
		return 3
	default:
		return -1
	}
}

func isTerminalStatus(st string) bool {
	return st == "FILLED" || st == "CANCELLED" || st == "REJECTED"
}

func markProcessedTx(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO processed_events (consumer_name, event_id)
		VALUES ($1, $2)
		ON CONFLICT (consumer_name, event_id) DO NOTHING
	`, ConsumerName, eventID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func sortUUIDs(ids []uuid.UUID) {
	sort.Slice(ids, func(i, j int) bool {
		return bytes.Compare(ids[i][:], ids[j][:]) < 0
	})
}

func (s *Service) sendDLQ(ctx context.Context, topic string, key, value []byte, cause error) error {
	if s.Log != nil {
		s.Log.Error("routing poison ledger event to DLQ", "topic", topic, "err", cause)
	}
	if s.Pub == nil {
		return nil
	}
	return s.Pub.Publish(ctx, []outbox.Message{{
		Topic:   kafka.DLQ(topic),
		Key:     key,
		Payload: value,
		Headers: map[string]string{"error": cause.Error()},
	}})
}

// RunKafkaLoop consumes trades.executed and orders.events, applies each event,
// and commits offsets only after the PostgreSQL transaction commits.
func (s *Service) RunKafkaLoop(ctx context.Context, cl *kgo.Client) error {
	for {
		fetches := cl.PollFetches(ctx)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return ctx.Err()
		}
		fetches.EachError(func(t string, part int32, err error) {
			s.Log.Warn("ledger fetch error", "topic", t, "partition", part, "err", err)
		})
		fetches.EachRecord(func(rec *kgo.Record) {
			for {
				err := s.HandleRaw(ctx, rec.Topic, rec.Key, rec.Value)
				if err == nil {
					break
				}
				if ctx.Err() != nil {
					return
				}
				s.Log.Error("ledger handle failed, retrying", "topic", rec.Topic, "offset", rec.Offset, "err", err)
				time.Sleep(200 * time.Millisecond)
			}
			if err := cl.CommitRecords(ctx, rec); err != nil && ctx.Err() == nil {
				s.Log.Warn("ledger commit failed", "topic", rec.Topic, "offset", rec.Offset, "err", err)
			}
		})
	}
}
