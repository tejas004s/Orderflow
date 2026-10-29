package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tejas004s/orderflow/internal/events"
	"github.com/tejas004s/orderflow/internal/kafka"
	"github.com/tejas004s/orderflow/internal/obs"
	"github.com/tejas004s/orderflow/internal/outbox"
)

// Errors specific to order replace.
var (
	// ErrOrderNotReplaceable: the order is terminal, is not a LIMIT order, or
	// a cancel has already been requested for it.
	ErrOrderNotReplaceable = errors.New("order cannot be replaced")
	// ErrReplacePending: an earlier replace of this order has not been
	// answered by the matching engine yet.
	ErrReplacePending = errors.New("a replace of this order is still in progress")
)

// ReplaceRequest amends a resting LIMIT order. A nil field keeps the current
// value; at least one must change. Qty is the new TOTAL quantity (filled +
// open), so it must exceed what has already been filled.
type ReplaceRequest struct {
	UserID  uuid.UUID `json:"-"`
	OrderID uuid.UUID `json:"-"`
	Price   *int64    `json:"price,omitempty"` // minor units
	Qty     *int64    `json:"qty,omitempty"`   // shares
}

// ReplaceResult is returned when a replace has been accepted for processing.
// The amendment itself takes effect asynchronously, when the matching engine
// answers; follow the order's updates (or GET it) for the outcome.
type ReplaceResult struct {
	// Order is the order as stored right now. Its price, qty and status still
	// show the pre-replace values until the engine confirms.
	Order     Order     `json:"order"`
	ReplaceID uuid.UUID `json:"replace_id"`
	NewPrice  int64     `json:"new_price"`
	NewQty    int64     `json:"new_qty"`
	// ReservedDelta is the extra cash (BUY, minor units) or shares (SELL)
	// moved into reservation by this request. Any surplus is released once the
	// engine confirms the replace.
	ReservedDelta int64  `json:"reserved_delta"`
	Status        string `json:"status"` // always "PENDING" here
}

// replaceReserveDelta returns how much additional cash (BUY) or how many
// additional shares (SELL) must be reserved up front for a replace.
//
// The order-service only knows the filled quantity the ledger has settled so
// far (filledDB). The matching engine may already have filled more by the time
// it processes the REPLACE, which changes how much reservation the new terms
// need. Per-share reservation is price*openQty for a BUY, so for an engine
// fill count F at the moment of the replace the reservation changes by
//
//	delta(F) = newPrice*(newQty-F) - prevPrice*(prevQty-F)
//
// which is linear in F. F lies in [filledDB, min(prevQty,newQty)-1], so its
// maximum is at one of the two ends. Reserving that maximum guarantees the
// ledger never has to take more cash/shares when the engine confirms: it can
// only release a surplus. A SELL reserves shares, not cash, so only the
// quantity change matters and F cancels out.
func replaceReserveDelta(side string, prevPrice, prevQty, newPrice, newQty, filledDB int64) int64 {
	if side != "BUY" {
		return max(0, newQty-prevQty)
	}
	delta := func(f int64) int64 { return newPrice*(newQty-f) - prevPrice*(prevQty-f) }
	fMax := max(filledDB, min(prevQty, newQty)-1)
	return max(0, delta(filledDB), delta(fMax))
}

// ReplaceOrder validates the amendment, reserves any extra funds/shares, and
// emits a REPLACE command through the outbox, all in one database transaction.
//
// Only reservation GROWTH is applied here. The order's stored price and
// quantity (and any release of surplus reservation) change when the ledger
// consumes the engine's answer, because only the engine knows how much of the
// order has really been filled. The one exception is qty: it is raised
// immediately (never lowered) so that fills the engine makes under the larger
// quantity can never exceed the stored quantity.
func (s *Service) ReplaceOrder(ctx context.Context, req ReplaceRequest) (ReplaceResult, error) {
	if req.UserID == uuid.Nil {
		return ReplaceResult{}, fmt.Errorf("%w: user_id is required", ErrValidation)
	}
	if req.Price == nil && req.Qty == nil {
		return ReplaceResult{}, fmt.Errorf("%w: provide price and/or qty", ErrValidation)
	}
	if req.Price != nil && *req.Price <= 0 {
		return ReplaceResult{}, fmt.Errorf("%w: price must be positive", ErrValidation)
	}
	if req.Qty != nil && *req.Qty <= 0 {
		return ReplaceResult{}, fmt.Errorf("%w: qty must be positive", ErrValidation)
	}

	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ReplaceResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Lock the order; every later check is made against this locked row.
	ord, err := getOrderForUpdateTx(ctx, tx, req.OrderID)
	if err != nil {
		return ReplaceResult{}, err
	}
	if ord.UserID != req.UserID {
		return ReplaceResult{}, ErrOrderNotFound
	}
	switch ord.Status {
	case "FILLED", "CANCELLED", "REJECTED":
		return ReplaceResult{}, fmt.Errorf("%w: status is %s", ErrOrderNotReplaceable, ord.Status)
	}
	if ord.Type != "LIMIT" || ord.Price == nil {
		return ReplaceResult{}, fmt.Errorf("%w: only LIMIT orders can be replaced", ErrOrderNotReplaceable)
	}
	var cancelRequested, pending bool
	if err := tx.QueryRow(ctx, `
		SELECT cancel_requested_at IS NOT NULL,
		       EXISTS (SELECT 1 FROM order_replaces WHERE order_id = $1 AND status = 'PENDING')
		FROM orders WHERE id = $1
	`, ord.ID).Scan(&cancelRequested, &pending); err != nil {
		return ReplaceResult{}, err
	}
	if cancelRequested {
		return ReplaceResult{}, fmt.Errorf("%w: a cancel has already been requested", ErrOrderNotReplaceable)
	}
	if pending {
		return ReplaceResult{}, ErrReplacePending
	}

	// 2. Resolve and validate the new terms.
	prevPrice, prevQty := *ord.Price, ord.Qty
	newPrice, newQty := prevPrice, prevQty
	if req.Price != nil {
		newPrice = *req.Price
	}
	if req.Qty != nil {
		newQty = *req.Qty
	}
	if newPrice == prevPrice && newQty == prevQty {
		return ReplaceResult{}, fmt.Errorf("%w: price and qty are unchanged", ErrValidation)
	}
	if newQty <= ord.FilledQty {
		return ReplaceResult{}, fmt.Errorf("%w: qty %d must exceed the %d already filled; cancel the order instead",
			ErrValidation, newQty, ord.FilledQty)
	}
	var inst Instrument
	err = tx.QueryRow(ctx, `
		SELECT symbol, name, tick_size, lot_size, max_order_qty, ref_price, status
		FROM instruments WHERE symbol = $1
	`, ord.Symbol).Scan(&inst.Symbol, &inst.Name, &inst.TickSize, &inst.LotSize,
		&inst.MaxOrderQty, &inst.RefPrice, &inst.Status)
	if err != nil {
		return ReplaceResult{}, err
	}
	if inst.Status != "ACTIVE" {
		return ReplaceResult{}, fmt.Errorf("%w: instrument %s is %s", ErrValidation, inst.Symbol, inst.Status)
	}
	if newQty > inst.MaxOrderQty {
		return ReplaceResult{}, fmt.Errorf("%w: qty %d exceeds max_order_qty %d", ErrValidation, newQty, inst.MaxOrderQty)
	}
	if newQty%inst.LotSize != 0 {
		return ReplaceResult{}, fmt.Errorf("%w: qty %d is not a multiple of lot_size %d", ErrValidation, newQty, inst.LotSize)
	}
	if newPrice%inst.TickSize != 0 {
		return ReplaceResult{}, fmt.Errorf("%w: price %d is not a multiple of tick_size %d", ErrValidation, newPrice, inst.TickSize)
	}

	// 3. Reserve whatever extra the new terms could need (SDD section 7.3).
	delta := replaceReserveDelta(ord.Side, prevPrice, prevQty, newPrice, newQty, ord.FilledQty)
	if delta > 0 {
		if ord.Side == "BUY" {
			var avail int64
			err = tx.QueryRow(ctx, `
				SELECT available FROM accounts WHERE user_id = $1 AND currency = 'USD' FOR UPDATE
			`, ord.UserID).Scan(&avail)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && avail < delta) {
				return ReplaceResult{}, fmt.Errorf("%w: need %d more cents, have %d", ErrInsufficientFunds, delta, avail)
			}
			if err != nil {
				return ReplaceResult{}, err
			}
			if _, err = tx.Exec(ctx, `
				UPDATE accounts SET available = available - $1, reserved = reserved + $1
				WHERE user_id = $2 AND currency = 'USD'
			`, delta, ord.UserID); err != nil {
				return ReplaceResult{}, err
			}
		} else {
			var free int64
			err = tx.QueryRow(ctx, `
				SELECT qty FROM positions WHERE user_id = $1 AND symbol = $2 FOR UPDATE
			`, ord.UserID, ord.Symbol).Scan(&free)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && free < delta) {
				return ReplaceResult{}, fmt.Errorf("%w: need %d more shares of %s, have %d", ErrInsufficientShares, delta, ord.Symbol, free)
			}
			if err != nil {
				return ReplaceResult{}, err
			}
			if _, err = tx.Exec(ctx, `
				UPDATE positions SET qty = qty - $1, reserved_qty = reserved_qty + $1
				WHERE user_id = $2 AND symbol = $3
			`, delta, ord.UserID, ord.Symbol); err != nil {
				return ReplaceResult{}, err
			}
		}
	}

	// 4. Record the pending replace and update the order's reservation.
	now := time.Now().UTC()
	cmdID := uuid.New()
	if _, err = tx.Exec(ctx, `
		UPDATE orders
		SET qty = GREATEST(qty, $2), reserved_amount = reserved_amount + $3, updated_at = $4
		WHERE id = $1
	`, ord.ID, newQty, delta, now); err != nil {
		return ReplaceResult{}, err
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO order_replaces (command_id, order_id, user_id, prev_price, prev_qty,
		                            new_price, new_qty, reserve_delta)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, cmdID, ord.ID, ord.UserID, prevPrice, prevQty, newPrice, newQty, delta); err != nil {
		return ReplaceResult{}, err
	}

	// 5. Outbox: the REPLACE command, keyed by symbol like every other command
	// so it is totally ordered after the order's PLACE.
	wireCmd := events.OrderCommand{
		CommandID: cmdID,
		Kind:      "REPLACE",
		Symbol:    ord.Symbol,
		OrderID:   ord.ID,
		UserID:    ord.UserID,
		Price:     newPrice,
		Qty:       newQty,
		Timestamp: now.UnixNano(),
	}
	corrID := obs.CorrelationID(ctx)
	payload, err := events.Wrap(cmdID, events.TypeOrderCommand, "order-service", corrID, now.UnixNano(), wireCmd)
	if err != nil {
		return ReplaceResult{}, err
	}
	if err = outbox.InsertTx(ctx, tx, ord.ID.String(), kafka.TopicOrderCommands, []byte(ord.Symbol), payload, map[string]string{
		"correlation_id": corrID,
		"event_type":     events.TypeOrderCommand,
	}); err != nil {
		return ReplaceResult{}, err
	}

	stored, err := getOrderTx(ctx, tx, ord.ID)
	if err != nil {
		return ReplaceResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReplaceResult{}, err
	}
	if s.Relay != nil {
		s.Relay.Wake()
	}
	return ReplaceResult{
		Order: stored, ReplaceID: cmdID, NewPrice: newPrice, NewQty: newQty,
		ReservedDelta: delta, Status: "PENDING",
	}, nil
}

