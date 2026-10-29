package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditReport summarizes the state verified by Reconcile.
type AuditReport struct {
	TransactionsChecked int   `json:"transactions_checked"`
	TotalCashSupply     int64 `json:"total_cash_supply"`
	TotalReservedCash   int64 `json:"total_reserved_cash"`
}

// Reconcile verifies all accounting invariants in PostgreSQL (SDD sections 6.2,
// 7, 11, 19):
//  1. Every (txn_id, asset) in ledger_entries has SUM(DEBIT) == SUM(CREDIT).
//  2. Every asset across the entire ledger_entries table nets to 0.
//  3. No account or position has negative available or reserved balances.
//  4. Every user's accounts.reserved equals SUM(orders.reserved_amount) for BUY
//     orders, and positions.reserved_qty equals SUM(orders.reserved_amount) for
//     SELL orders.
func Reconcile(ctx context.Context, pool *pgxpool.Pool) (AuditReport, error) {
	var rep AuditReport
	var errs []error

	// 1. Per-transaction double-entry balance check.
	var unbalancedTxns int
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT txn_id, asset,
			       SUM(CASE WHEN direction = 'DEBIT'  THEN amount ELSE 0 END) AS debits,
			       SUM(CASE WHEN direction = 'CREDIT' THEN amount ELSE 0 END) AS credits
			FROM ledger_entries
			GROUP BY txn_id, asset
			HAVING SUM(CASE WHEN direction = 'DEBIT'  THEN amount ELSE 0 END) <>
			       SUM(CASE WHEN direction = 'CREDIT' THEN amount ELSE 0 END)
		) t
	`).Scan(&unbalancedTxns)
	if err != nil {
		return rep, err
	}
	if unbalancedTxns > 0 {
		errs = append(errs, fmt.Errorf("%d unbalanced (txn_id, asset) pairs in ledger_entries", unbalancedTxns))
	}

	_ = pool.QueryRow(ctx, `SELECT COUNT(DISTINCT txn_id) FROM ledger_entries`).Scan(&rep.TransactionsChecked)

	// 2. Global per-asset double-entry check.
	rows, err := pool.Query(ctx, `
		SELECT asset,
		       SUM(CASE WHEN direction = 'DEBIT'  THEN amount ELSE -amount END) AS net
		FROM ledger_entries
		GROUP BY asset
		HAVING SUM(CASE WHEN direction = 'DEBIT'  THEN amount ELSE -amount END) <> 0
	`)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var asset string
		var net int64
		if err := rows.Scan(&asset, &net); err != nil {
			rows.Close()
			return rep, err
		}
		errs = append(errs, fmt.Errorf("asset %s has non-zero net ledger balance %d", asset, net))
	}
	rows.Close()

	// 3. Negative balance check (belt-and-suspenders alongside CHECK constraints).
	var negAccounts, negPositions int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM accounts WHERE available < 0 OR reserved < 0`).Scan(&negAccounts); err != nil {
		return rep, err
	}
	if negAccounts > 0 {
		errs = append(errs, fmt.Errorf("%d accounts have negative balances", negAccounts))
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM positions WHERE qty < 0 OR reserved_qty < 0`).Scan(&negPositions); err != nil {
		return rep, err
	}
	if negPositions > 0 {
		errs = append(errs, fmt.Errorf("%d positions have negative quantities", negPositions))
	}

	// 4. Account cash reservation matches sum of open BUY order reservations.
	rows, err = pool.Query(ctx, `
		SELECT a.user_id, a.reserved, COALESCE(SUM(o.reserved_amount), 0) AS order_reserved
		FROM accounts a
		LEFT JOIN orders o
		  ON o.user_id = a.user_id AND o.side = 'BUY'
		GROUP BY a.user_id, a.reserved
		HAVING a.reserved <> COALESCE(SUM(o.reserved_amount), 0)
	`)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var uid uuid.UUID
		var acctRes, ordRes int64
		if err := rows.Scan(&uid, &acctRes, &ordRes); err != nil {
			rows.Close()
			return rep, err
		}
		errs = append(errs, fmt.Errorf("user %s account.reserved=%d != buy orders reserved_amount=%d", uid, acctRes, ordRes))
	}
	rows.Close()

	// 5. Position share reservation matches sum of open SELL order reservations.
	rows, err = pool.Query(ctx, `
		SELECT p.user_id, p.symbol, p.reserved_qty, COALESCE(SUM(o.reserved_amount), 0) AS order_reserved
		FROM positions p
		LEFT JOIN orders o
		  ON o.user_id = p.user_id AND o.symbol = p.symbol AND o.side = 'SELL'
		GROUP BY p.user_id, p.symbol, p.reserved_qty
		HAVING p.reserved_qty <> COALESCE(SUM(o.reserved_amount), 0)
	`)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var uid uuid.UUID
		var sym string
		var posRes, ordRes int64
		if err := rows.Scan(&uid, &sym, &posRes, &ordRes); err != nil {
			rows.Close()
			return rep, err
		}
		errs = append(errs, fmt.Errorf("user %s %s position.reserved_qty=%d != sell orders reserved_amount=%d", uid, sym, posRes, ordRes))
	}
	rows.Close()

	_ = pool.QueryRow(ctx, `SELECT COALESCE(SUM(available + reserved), 0), COALESCE(SUM(reserved), 0) FROM accounts`).
		Scan(&rep.TotalCashSupply, &rep.TotalReservedCash)

	return rep, errors.Join(errs...)
}
