#!/usr/bin/env bash
# OrderFlow Chaos Test (SDD section 14 & Milestone M7):
# Kills the matching-engine container mid-stream with SIGKILL (kill -9) while
# bots and orders are flowing, restarts it, waits for readiness (< 10s target),
# and runs `ofctl reconcile` to verify zero lost or duplicated trades and a
# balanced double-entry ledger.
set -euo pipefail

COMPOSE="docker compose -f deploy/docker-compose.yml"

echo "[chaos] 1/4 Ensuring stack is up and bots are trading..."
$COMPOSE up -d --wait

sleep 3

echo "[chaos] 2/4 Sending SIGKILL (kill -9) to matching-engine mid-stream..."
t_kill=$(date +%s%3N)
$COMPOSE kill -s SIGKILL matching

echo "[chaos] 3/4 Restarting matching-engine and waiting for /readyz..."
$COMPOSE up -d --wait matching
t_ready=$(date +%s%3N)
recovery_ms=$((t_ready - t_kill))
echo "[chaos] matching-engine recovered in ${recovery_ms} ms (target < 10000 ms)"

sleep 2

echo "[chaos] 4/4 Auditing double-entry ledger and reservations via ofctl reconcile..."
$COMPOSE exec -T ledger ofctl reconcile
echo "[chaos] PASS: zero lost/duplicated trades, ledger balanced to zero."
