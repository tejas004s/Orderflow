// Package order implements order validation, fund/share reservation,
// idempotency-key enforcement, and transactional outbox emission
// (SDD sections 6.2, 7.1, 7.3).
package order

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tejas004s/orderflow/internal/events"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/obs"
	"github.com/tejas004s/orderflow/internal/outbox"
)

// Domain errors returned by Service and mapped to RFC 7807 status codes.
var (
	ErrValidation          = errors.New("validation error")
	ErrInsufficientFunds   = errors.New("insufficient available funds")
	ErrInsufficientShares  = errors.New("insufficient available shares")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different parameters")
	ErrOrderNotFound       = errors.New("order not found")
	ErrOrderNotCancelable  = errors.New("order is already in a terminal state")
)

// PlaceRequest is the input to PlaceOrder.
type PlaceRequest struct {
	UserID         uuid.UUID `json:"user_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Symbol         string    `json:"symbol"`
	Side           string    `json:"side"`  // "BUY" | "SELL"
	Type           string    `json:"type"`  // "LIMIT" | "MARKET"
	Price          int64     `json:"price"` // minor units; 0 for MARKET
	Qty            int64     `json:"qty"`   // shares
}

// Order is the persisted representation of a user's order.
type Order struct {
	ID              uuid.UUID `json:"id"`
	UserID          uuid.UUID `json:"user_id"`
	Symbol          string    `json:"symbol"`
	Side            string    `json:"side"`
	Type            string    `json:"type"`
	Price           *int64    `json:"price,omitempty"`
	ProtectionPrice *int64    `json:"protection_price,omitempty"`
	Qty             int64     `json:"qty"`
	FilledQty       int64     `json:"filled_qty"`
	ReservedAmount  int64     `json:"reserved_amount"`
	Status          string    `json:"status"`
	RejectReason    *string   `json:"reject_reason,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Instrument is a tradable symbol from the instruments table.
type Instrument struct {
	Symbol      string `json:"symbol"`
	Name        string `json:"name"`
	TickSize    int64  `json:"tick_size"`
	LotSize     int64  `json:"lot_size"`
	MaxOrderQty int64  `json:"max_order_qty"`
	RefPrice    int64  `json:"ref_price"`
	Status      string `json:"status"`
}

// Service manages order placement and cancellation.
type Service struct {
	Pool  *pgxpool.Pool
	Relay *outbox.Relay // optional: woken after commit
}

// NewService creates an order Service.
func NewService(pool *pgxpool.Pool, relay *outbox.Relay) *Service {
	return &Service{Pool: pool, Relay: relay}
}

func requestHash(req PlaceRequest) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d|%d",
		req.Symbol, req.Side, req.Type, req.Price, req.Qty)))
	return sum[:]
}

// ComputeProtectionPrice calculates a 5% upper price band for market buys,
// rounded up to a multiple of tickSize.
func ComputeProtectionPrice(refPrice, tickSize int64) int64 {
	if tickSize <= 0 {
		tickSize = 1
	}
	// ceil(refPrice * 1.05) = (refPrice*105 + 99) / 100
	banded := (refPrice*105 + 99) / 100
	if rem := banded % tickSize; rem != 0 {
		banded += tickSize - rem
	}
	return banded
}

// PlaceOrder validates req, reserves cash or shares, persists the order as NEW,
// and writes the orders.commands outbox row in a single database transaction.
func (s *Service) PlaceOrder(ctx context.Context, req PlaceRequest) (Order, bool, error) {
	req.Symbol = strings.ToUpper(strings.TrimSpace(req.Symbol))
	req.Side = strings.ToUpper(strings.TrimSpace(req.Side))
	req.Type = strings.ToUpper(strings.TrimSpace(req.Type))
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)

	if req.UserID == uuid.Nil {
		return Order{}, false, fmt.Errorf("%w: user_id is required", ErrValidation)
	}
	if req.IdempotencyKey == "" {
		return Order{}, false, fmt.Errorf("%w: Idempotency-Key header is required", ErrValidation)
	}
	if req.Side != "BUY" && req.Side != "SELL" {
		return Order{}, false, fmt.Errorf("%w: side must be BUY or SELL", ErrValidation)
	}
	if req.Type != "LIMIT" && req.Type != "MARKET" {
		return Order{}, false, fmt.Errorf("%w: type must be LIMIT or MARKET", ErrValidation)
	}
	if req.Qty <= 0 {
		return Order{}, false, fmt.Errorf("%w: qty must be positive", ErrValidation)
	}
	if req.Type == "LIMIT" && req.Price <= 0 {
		return Order{}, false, fmt.Errorf("%w: limit order requires positive price", ErrValidation)
	}
	if req.Type == "MARKET" && req.Price != 0 {
		return Order{}, false, fmt.Errorf("%w: market order must not specify price", ErrValidation)
	}

	reqHash := requestHash(req)

	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Order{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Check idempotency key (lock user's idempotency row if present).
	var existingOrderID uuid.UUID
	var existingHash []byte
	err = tx.QueryRow(ctx, `
		SELECT order_id, request_hash
		FROM idempotency_keys
		WHERE user_id = $1 AND key = $2
	`, req.UserID, req.IdempotencyKey).Scan(&existingOrderID, &existingHash)
	if err == nil {
		if !bytes.Equal(existingHash, reqHash) {
			return Order{}, false, ErrIdempotencyConflict
		}
		ord, err := getOrderTx(ctx, tx, existingOrderID)
		if err != nil {
			return Order{}, false, err
		}
		return ord, true, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, err
	}

	// 2. Load and validate instrument.
	var inst Instrument
	err = tx.QueryRow(ctx, `
		SELECT symbol, name, tick_size, lot_size, max_order_qty, ref_price, status
		FROM instruments
		WHERE symbol = $1
	`, req.Symbol).Scan(&inst.Symbol, &inst.Name, &inst.TickSize, &inst.LotSize,
		&inst.MaxOrderQty, &inst.RefPrice, &inst.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, false, fmt.Errorf("%w: unknown symbol %s", ErrValidation, req.Symbol)
	}
	if err != nil {
		return Order{}, false, err
	}
	if inst.Status != "ACTIVE" {
		return Order{}, false, fmt.Errorf("%w: instrument %s is %s", ErrValidation, inst.Symbol, inst.Status)
	}
	if req.Qty > inst.MaxOrderQty {
		return Order{}, false, fmt.Errorf("%w: qty %d exceeds max_order_qty %d", ErrValidation, req.Qty, inst.MaxOrderQty)
	}
	if req.Qty%inst.LotSize != 0 {
		return Order{}, false, fmt.Errorf("%w: qty %d is not a multiple of lot_size %d", ErrValidation, req.Qty, inst.LotSize)
	}
	if req.Type == "LIMIT" && req.Price%inst.TickSize != 0 {
		return Order{}, false, fmt.Errorf("%w: price %d is not a multiple of tick_size %d", ErrValidation, req.Price, inst.TickSize)
	}

	// 3. Compute reservation and lock account or position row (SDD section 7.3).
	var (
		limitPrice      *int64
		protectionPrice *int64
		cmdPrice        int64
		reservedAmount  int64
	)
	if req.Type == "LIMIT" {
		px := req.Price
		limitPrice = &px
		cmdPrice = px
	} else if req.Side == "BUY" {
		// Use latest executed trade price if available, otherwise instrument ref_price.
		refPx := inst.RefPrice
		_ = tx.QueryRow(ctx, `
			SELECT price FROM trades WHERE symbol = $1 ORDER BY executed_at DESC LIMIT 1
		`, req.Symbol).Scan(&refPx)
		band := ComputeProtectionPrice(refPx, inst.TickSize)
		protectionPrice = &band
		cmdPrice = band
	}

	if req.Side == "BUY" {
		reservedAmount = cmdPrice * req.Qty
		var avail int64
		err = tx.QueryRow(ctx, `
			SELECT available FROM accounts
			WHERE user_id = $1 AND currency = 'USD'
			FOR UPDATE
		`, req.UserID).Scan(&avail)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && avail < reservedAmount) {
			return Order{}, false, fmt.Errorf("%w: need %d cents, have %d", ErrInsufficientFunds, reservedAmount, avail)
		}
		if err != nil {
			return Order{}, false, err
		}
		_, err = tx.Exec(ctx, `
			UPDATE accounts
			SET available = available - $1, reserved = reserved + $1
			WHERE user_id = $2 AND currency = 'USD'
		`, reservedAmount, req.UserID)
		if err != nil {
			return Order{}, false, err
		}
	} else {
		reservedAmount = req.Qty
		var freeShares int64
		err = tx.QueryRow(ctx, `
			SELECT qty FROM positions
			WHERE user_id = $1 AND symbol = $2
			FOR UPDATE
		`, req.UserID, req.Symbol).Scan(&freeShares)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && freeShares < reservedAmount) {
			return Order{}, false, fmt.Errorf("%w: need %d shares of %s, have %d", ErrInsufficientShares, reservedAmount, req.Symbol, freeShares)
		}
		if err != nil {
			return Order{}, false, err
		}
		_, err = tx.Exec(ctx, `
			UPDATE positions
			SET qty = qty - $1, reserved_qty = reserved_qty + $1
			WHERE user_id = $2 AND symbol = $3
		`, reservedAmount, req.UserID, req.Symbol)
		if err != nil {
			return Order{}, false, err
		}
	}

	// 4. Insert order as NEW + idempotency key + outbox command.
	orderID := uuid.New()
	now := time.Now().UTC()
	var ord Order
	err = tx.QueryRow(ctx, `
		INSERT INTO orders (
			id, user_id, symbol, side, type, price, protection_price,
			qty, filled_qty, reserved_amount, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, $9, 'NEW', $10, $10)
		RETURNING id, user_id, symbol, side, type, price, protection_price,
		          qty, filled_qty, reserved_amount, status, reject_reason, created_at, updated_at
	`, orderID, req.UserID, req.Symbol, req.Side, req.Type, limitPrice, protectionPrice,
		req.Qty, reservedAmount, now).Scan(
		&ord.ID, &ord.UserID, &ord.Symbol, &ord.Side, &ord.Type, &ord.Price, &ord.ProtectionPrice,
		&ord.Qty, &ord.FilledQty, &ord.ReservedAmount, &ord.Status, &ord.RejectReason,
		&ord.CreatedAt, &ord.UpdatedAt,
	)
	if err != nil {
		return Order{}, false, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO idempotency_keys (user_id, key, request_hash, order_id)
		VALUES ($1, $2, $3, $4)
	`, req.UserID, req.IdempotencyKey, reqHash, orderID)
	if err != nil {
		return Order{}, false, err
	}

	cmdID := uuid.New()
	wireCmd := events.OrderCommand{
		CommandID: cmdID,
		Kind:      "PLACE",
		Symbol:    req.Symbol,
		OrderID:   orderID,
		UserID:    req.UserID,
		Side:      req.Side,
		OrderType: req.Type,
		Price:     cmdPrice,
		Qty:       req.Qty,
		Timestamp: now.UnixNano(),
	}
	corrID := obs.CorrelationID(ctx)
	payload, err := events.Wrap(cmdID, events.TypeOrderCommand, "order-service", corrID, now.UnixNano(), wireCmd)
	if err != nil {
		return Order{}, false, err
	}
	err = outbox.InsertTx(ctx, tx, orderID.String(), kafka.TopicOrderCommands, []byte(req.Symbol), payload, map[string]string{
		"correlation_id": corrID,
		"event_type":     events.TypeOrderCommand,
	})
	if err != nil {
		return Order{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Order{}, false, err
	}
	if s.Relay != nil {
		s.Relay.Wake()
	}
	return ord, false, nil
}

// CancelOrder validates ownership and emits a CANCEL command via the outbox.
func (s *Service) CancelOrder(ctx context.Context, userID, orderID uuid.UUID) (Order, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Order{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ord, err := getOrderForUpdateTx(ctx, tx, orderID)
	if err != nil {
		return Order{}, err
	}
	if ord.UserID != userID {
		return Order{}, ErrOrderNotFound
	}
	switch ord.Status {
	case "FILLED", "CANCELLED", "REJECTED":
		return Order{}, fmt.Errorf("%w: status is %s", ErrOrderNotCancelable, ord.Status)
	}

	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `
		UPDATE orders
		SET cancel_requested_at = COALESCE(cancel_requested_at, $1), updated_at = $1
		WHERE id = $2
	`, now, orderID)
	if err != nil {
		return Order{}, err
	}

	cmdID := uuid.New()
	wireCmd := events.OrderCommand{
		CommandID: cmdID,
		Kind:      "CANCEL",
		Symbol:    ord.Symbol,
		OrderID:   ord.ID,
		UserID:    ord.UserID,
		Timestamp: now.UnixNano(),
	}
	corrID := obs.CorrelationID(ctx)
	payload, err := events.Wrap(cmdID, events.TypeOrderCommand, "order-service", corrID, now.UnixNano(), wireCmd)
	if err != nil {
		return Order{}, err
	}
	err = outbox.InsertTx(ctx, tx, ord.ID.String(), kafka.TopicOrderCommands, []byte(ord.Symbol), payload, map[string]string{
		"correlation_id": corrID,
		"event_type":     events.TypeOrderCommand,
	})
	if err != nil {
		return Order{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Order{}, err
	}
	if s.Relay != nil {
		s.Relay.Wake()
	}
	return ord, nil
}

// GetOrder loads a single order for a user.
func (s *Service) GetOrder(ctx context.Context, userID, orderID uuid.UUID) (Order, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return Order{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ord, err := getOrderTx(ctx, tx, orderID)
	if err != nil {
		return Order{}, err
	}
	if ord.UserID != userID {
		return Order{}, ErrOrderNotFound
	}
	return ord, nil
}

// ListOrders returns a user's orders filtered by optional status and symbol.
func (s *Service) ListOrders(ctx context.Context, userID uuid.UUID, status, symbol string, limit int) ([]Order, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, user_id, symbol, side, type, price, protection_price,
		       qty, filled_qty, reserved_amount, status, reject_reason, created_at, updated_at
		FROM orders
		WHERE user_id = $1
		  AND ($2 = '' OR status = $2)
		  AND ($3 = '' OR symbol = $3)
		ORDER BY created_at DESC, id DESC
		LIMIT $4
	`, userID, strings.ToUpper(status), strings.ToUpper(symbol), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Order{}
	for rows.Next() {
		var o Order
		if err := rows.Scan(
			&o.ID, &o.UserID, &o.Symbol, &o.Side, &o.Type, &o.Price, &o.ProtectionPrice,
			&o.Qty, &o.FilledQty, &o.ReservedAmount, &o.Status, &o.RejectReason,
			&o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func getOrderTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Order, error) {
	return scanOrder(tx.QueryRow(ctx, `
		SELECT id, user_id, symbol, side, type, price, protection_price,
		       qty, filled_qty, reserved_amount, status, reject_reason, created_at, updated_at
		FROM orders WHERE id = $1
	`, id))
}

func getOrderForUpdateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Order, error) {
	return scanOrder(tx.QueryRow(ctx, `
		SELECT id, user_id, symbol, side, type, price, protection_price,
		       qty, filled_qty, reserved_amount, status, reject_reason, created_at, updated_at
		FROM orders WHERE id = $1 FOR UPDATE
	`, id))
}

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(
		&o.ID, &o.UserID, &o.Symbol, &o.Side, &o.Type, &o.Price, &o.ProtectionPrice,
		&o.Qty, &o.FilledQty, &o.ReservedAmount, &o.Status, &o.RejectReason,
		&o.CreatedAt, &o.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrOrderNotFound
	}
	return o, err
}
