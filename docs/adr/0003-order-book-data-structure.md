# ADR-003: Order Book Data Structure (B-Tree of Price Levels + Intrusive FIFO Queue)

- **Status:** Accepted
- **Date:** 2026-10-28

## Context

Each symbol's order book must support:
1. Finding the best bid (max price) and best ask (min price) in $O(1)$ or $O(\log L)$ where $L$ is the number of active price levels.
2. Appending a new resting order to a price level in $O(1)$ once the level is located.
3. Cancelling any resting order by `order_id` in $O(1)$ without scanning the level.
4. Iterating the top $K$ price levels in sorted order to produce L2 depth snapshots.

We implemented and benchmarked two level-index candidates in `internal/engine`:
- **`IndexBTree`**: `tidwall/btree` generic B-Tree (`Degree: 64`, `NoLocks: true`) paired with a `map[int64]*level` for $O(1)$ price lookup.
- **`IndexHeap`**: binary `container/heap` with lazy deletion (`dead` flag on emptied levels, periodic compaction) paired with `map[int64]*level`.

Within each level, resting orders are linked in an intrusive doubly-linked FIFO queue (`head`/`tail` pointers on `level`, `prev`/`next` on `order`), and a book-wide `map[uuid.UUID]*order` enables $O(1)$ cancel.

## Decision

Use **`IndexBTree`** as the default price-level index, while retaining `IndexHeap` behind `WithIndex(IndexHeap)` for differential property testing and benchmarking.

## Consequences

- **Pros:** Ordered top-$K$ depth queries (`Book.Depth(20)`) walk the B-Tree in $O(K)$ with zero sorting allocations, whereas `IndexHeap` must copy and sort all active levels ($O(L \log L)$) on every depth snapshot. Cache locality of `Degree: 64` B-Tree nodes keeps matching throughput competitive with a binary heap.
- **Cons:** Slightly higher per-level insertion cost when a brand-new price level is created compared to a binary heap push.

