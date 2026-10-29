package ledger

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tejas004s/orderflow/internal/events"
)

// Order replace settlement (docs/adr/0007-order-replace.md).
//
// order-service reserves the worst-case EXTRA cash/shares when it accepts a
// replace and records it in order_replaces.reserve_delta. The matching engine
// then answers with exactly one of:
//
//	REPLACED        the order now has ev.Price / ev.Qty.
//	REPLACE_REJECT  nothing changed.
//
// Both are applied here, in the same transaction that marks the event
// processed, so redelivery is harmless.

// settleReplaced applies a REPLACED event and returns the order's new
// reserved_amount (the caller persists it together with the order's status).
//
// The engine reports the order's state immediately before the replace
// (PrevPrice/PrevQty/PrevFilled), so the change in what the order needs
// reserved is fully determined by the event, independent of how many trades
// the ledger has settled so far:
//
//	BUY : newPrice*(newQty-prevFilled) - prevPrice*(prevQty-prevFilled)
//	SELL: newQty - prevQty
//
// Whatever order-service already reserved is subtracted, leaving a surplus to
// release. Trades are settled at the buy limit the engine matched them at
// (TradeExecuted.BuyLimitPrice), so they commute with this adjustment.
func settleReplaced(ctx context.Context, tx pgx.Tx, ev events.OrderEvent, userID uuid.UUID, symbol, side string, reserved int64) (int64, error) {
	var already int64
	err := tx.QueryRow(ctx, `
		SELECT reserve_delta FROM order_replaces
		WHERE command_id = $1 AND order_id = $2
		FOR UPDATE
	`, ev.CommandID, ev.OrderID).Scan(&already)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	prevLeaves := ev.PrevQty - ev.PrevFilled
	newLeaves := ev.Qty - ev.PrevFilled
	need := newLeaves - prevLeaves
	if side == "BUY" {
		need = ev.Price*newLeaves - ev.PrevPrice*prevLeaves
	}
	reserved, err = adjustReservation(ctx, tx, side, userID, symbol, need-already, reserved)
	if err != nil {
		return 0, err
	}

	if _, err = tx.Exec(ctx, `
		UPDATE orders SET price = $2, qty = $3, updated_at = now() WHERE id = $1
	`, ev.OrderID, ev.Price, ev.Qty); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `
		UPDATE order_replaces SET status = 'APPLIED', resolved_at = now()
		WHERE command_id = $1 AND status = 'PENDING'
	`, ev.CommandID); err != nil {
		return 0, err
	}
	return reserved, nil
}

// settleReplaceReject undoes what order-service did when it accepted the
// replace: it returns the extra reservation and restores the quantity.
func settleReplaceReject(ctx context.Context, tx pgx.Tx, ev events.OrderEvent, userID uuid.UUID, symbol, side string, reserved int64) error {
	var (
		extra, prevQty int64
		status         string
	)
	err := tx.QueryRow(ctx, `
		SELECT reserve_delta, prev_qty, status FROM order_replaces
		WHERE command_id = $1 AND order_id = $2
		FOR UPDATE
	`, ev.CommandID, ev.OrderID).Scan(&extra, &prevQty, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not issued through order-service: nothing was reserved
	}
	if err != nil {
		return err
	}
	if status != "PENDING" {
		return nil
	}

	reserved, err = adjustReservation(ctx, tx, side, userID, symbol, -extra, reserved)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `
		UPDATE orders SET qty = $2, reserved_amount = $3, updated_at = now() WHERE id = $1
	`, ev.OrderID, prevQty, reserved); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE order_replaces SET status = 'REJECTED', reject_reason = $2, resolved_at = now()
		WHERE command_id = $1
	`, ev.CommandID, ev.Reason)
	return err
}

// adjustReservation moves delta units of cash (BUY) or shares (SELL) between
// the user's free balance and this order's reservation: delta > 0 reserves
// more, delta < 0 releases. It returns the order's new reserved_amount.
//
// The amount is clamped to what is actually available (to release: what the
// order still holds; to reserve: the free balance) so a bookkeeping surprise
// can never push a balance negative and wedge the consumer in its retry loop.
// order-service reserves the worst case up front, so in normal operation the
// clamp never bites and delta is never positive.
func adjustReservation(ctx context.Context, tx pgx.Tx, side string, userID uuid.UUID, symbol string, delta, reserved int64) (int64, error) {
	if delta == 0 {
		return reserved, nil
	}
	if delta < 0 {
		delta = -min(-delta, reserved)
	} else {
		var free int64
		var err error
		if side == "BUY" {
			err = tx.QueryRow(ctx, `
				SELECT available FROM accounts WHERE user_id = $1 AND currency = 'USD' FOR UPDATE
			`, userID).Scan(&free)
		} else {
			err = tx.QueryRow(ctx, `
				SELECT qty FROM positions WHERE user_id = $1 AND symbol = $2 FOR UPDATE
			`, userID, symbol).Scan(&free)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return reserved, nil
		}
		if err != nil {
			return 0, err
		}
		delta = min(delta, free)
	}
	if delta == 0 {
		return reserved, nil
	}

	var err error
	if side == "BUY" {
		_, err = tx.Exec(ctx, `
			UPDATE accounts SET available = available - $1, reserved = reserved + $1
			WHERE user_id = $2 AND currency = 'USD'
		`, delta, userID)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE positions SET qty = qty - $1, reserved_qty = reserved_qty + $1
			WHERE user_id = $2 AND symbol = $3
		`, delta, userID, symbol)
	}
	if err != nil {
		return 0, err
	}
	return reserved + delta, nil
}

