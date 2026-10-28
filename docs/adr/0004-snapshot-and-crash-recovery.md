# ADR-004: Matching Engine Snapshot and Deterministic Crash Recovery

- **Status:** Accepted
- **Date:** 2026-10-28

## Context

The matching engine holds order books in memory. On restart after a crash or deployment, replaying `orders.commands` from offset 0 would take too long once millions of commands have accumulated (NFR target: restart to ready < 10 s). Furthermore, replaying commands after a crash between producing output events and committing the consumer offset must not create duplicate trades downstream.

## Decision

1. **Pure determinism:** `Engine.Process(cmd)` never reads the system clock or random number generator. Timestamps come from `cmd.Timestamp`, and every output event's UUID is derived deterministically as `UUIDv5(nsEvent, cmd.ID || eventIndex)`.
2. **Periodic snapshots:** Each partition worker periodically serializes its `Engine` state (`Engine.Snapshot()`) along with `next_offset` (the first Kafka offset not included in the snapshot) into PostgreSQL table `engine_snapshots(partition, next_offset, snapshot, taken_at)`.
3. **Recovery:** On startup, the partition worker loads the row for its partition, restores the `Engine`, seeks its Kafka consumer to `next_offset`, and replays the tail. Any events re-emitted during replay carry the exact same deterministic `event_id` / `match_id` and are discarded by downstream `processed_events` / `UNIQUE(match_id)` constraints.

## Consequences

- **Pros:** Sub-second recovery regardless of total topic history; zero lost or duplicated trades under `kill -9`.
- **Cons:** Snapshot serialization briefly pauses the partition worker for a few milliseconds every $N$ commands.

