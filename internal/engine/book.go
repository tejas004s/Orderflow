package engine

import (
	"container/heap"
	"sort"

	"github.com/google/uuid"
	"github.com/tidwall/btree"
)

// order is a resting order. Orders at one price level form an intrusive
// doubly-linked FIFO list, so removal (cancel / full fill) is O(1).
type order struct {
	id     uuid.UUID
	user   uuid.UUID
	side   Side
	price  int64
	qty    int64
	filled int64
	ts     int64

	lvl        *level
	prev, next *order
}

func (o *order) leaves() int64 { return o.qty - o.filled }

// level is all resting orders at one price on one side.
type level struct {
	price int64
	total int64 // sum of leaves of orders in the level
	count int
	head  *order
	tail  *order

	// heap index bookkeeping (used only by heapIndex)
	heapPos int
	dead    bool
}

func (l *level) push(o *order) {
	o.lvl = l
	o.prev = l.tail
	o.next = nil
	if l.tail != nil {
		l.tail.next = o
	} else {
		l.head = o
	}
	l.tail = o
	l.total += o.leaves()
	l.count++
}

func (l *level) unlink(o *order) {
	if o.prev != nil {
		o.prev.next = o.next
	} else {
		l.head = o.next
	}
	if o.next != nil {
		o.next.prev = o.prev
	} else {
		l.tail = o.prev
	}
	l.total -= o.leaves()
	l.count--
	o.prev, o.next, o.lvl = nil, nil, nil
}

// IndexKind selects the price-level index implementation.
type IndexKind uint8

const (
	// IndexBTree keeps levels in a B-tree: O(log L) insert/delete, O(log L)
	// best, ordered iteration for depth snapshots. This is the default.
	IndexBTree IndexKind = iota
	// IndexHeap keeps levels in a binary heap with lazy deletion: cheap
	// insert and best, but depth snapshots require sorting. Kept for the
	// benchmark comparison in ADR-003.
	IndexHeap
)

// levelIndex is a sorted collection of price levels for one side. "better"
// means higher price for bids, lower price for asks.
type levelIndex interface {
	best() *level
	get(price int64) *level
	add(l *level)
	remove(l *level)
	len() int
	// ascend visits levels from best to worst until fn returns false.
	ascend(fn func(*level) bool)
}

func newIndex(kind IndexKind, side Side) levelIndex {
	better := func(a, b int64) bool { return a > b } // bids: higher is better
	if side == Sell {
		better = func(a, b int64) bool { return a < b }
	}
	if kind == IndexHeap {
		return &heapIndex{h: levelHeap{better: better}, byPx: map[int64]*level{}}
	}
	return &btreeIndex{
		tree: btree.NewBTreeGOptions(func(a, b *level) bool { return better(a.price, b.price) },
			btree.Options{NoLocks: true, Degree: 64}),
		byPx: map[int64]*level{},
	}
}

// ---- B-tree implementation ------------------------------------------------

type btreeIndex struct {
	tree *btree.BTreeG[*level]
	byPx map[int64]*level
}

func (b *btreeIndex) best() *level {
	l, ok := b.tree.Min()
	if !ok {
		return nil
	}
	return l
}
func (b *btreeIndex) get(p int64) *level { return b.byPx[p] }
func (b *btreeIndex) add(l *level)       { b.tree.Set(l); b.byPx[l.price] = l }
func (b *btreeIndex) remove(l *level)    { b.tree.Delete(l); delete(b.byPx, l.price) }
func (b *btreeIndex) len() int           { return len(b.byPx) }
func (b *btreeIndex) ascend(fn func(*level) bool) {
	b.tree.Scan(fn)
}

// ---- Heap implementation (lazy deletion) ---------------------------------

type levelHeap struct {
	items  []*level
	better func(a, b int64) bool
}

func (h levelHeap) Len() int           { return len(h.items) }
func (h levelHeap) Less(i, j int) bool { return h.better(h.items[i].price, h.items[j].price) }
func (h levelHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].heapPos = i
	h.items[j].heapPos = j
}
func (h *levelHeap) Push(x any) {
	l := x.(*level)
	l.heapPos = len(h.items)
	h.items = append(h.items, l)
}
func (h *levelHeap) Pop() any {
	n := len(h.items)
	l := h.items[n-1]
	h.items[n-1] = nil
	h.items = h.items[:n-1]
	return l
}

type heapIndex struct {
	h     levelHeap
	byPx  map[int64]*level
	nDead int
}

func (x *heapIndex) best() *level {
	for x.h.Len() > 0 {
		top := x.h.items[0]
		if !top.dead {
			return top
		}
		heap.Pop(&x.h)
		x.nDead--
	}
	return nil
}
func (x *heapIndex) get(p int64) *level { return x.byPx[p] }
func (x *heapIndex) add(l *level) {
	l.dead = false
	heap.Push(&x.h, l)
	x.byPx[l.price] = l
}
func (x *heapIndex) remove(l *level) {
	// Lazy deletion: mark dead, drop from the price map, and let best()
	// discard it when it surfaces. Compact if garbage dominates.
	l.dead = true
	x.nDead++
	delete(x.byPx, l.price)
	if x.nDead > 64 && x.nDead > len(x.byPx) {
		live := x.h.items[:0]
		for _, it := range x.h.items {
			if !it.dead {
				live = append(live, it)
			}
		}
		for i := len(live); i < len(x.h.items); i++ {
			x.h.items[i] = nil
		}
		x.h.items = live
		for i, it := range x.h.items {
			it.heapPos = i
		}
		heap.Init(&x.h)
		x.nDead = 0
	}
}
func (x *heapIndex) len() int { return len(x.byPx) }
func (x *heapIndex) ascend(fn func(*level) bool) {
	ls := make([]*level, 0, len(x.byPx))
	for _, l := range x.byPx {
		ls = append(ls, l)
	}
	sort.Slice(ls, func(i, j int) bool { return x.h.better(ls[i].price, ls[j].price) })
	for _, l := range ls {
		if !fn(l) {
			return
		}
	}
}

// ---- Book -----------------------------------------------------------------

// Book is the order book for a single symbol.
type Book struct {
	Symbol string
	bids   levelIndex
	asks   levelIndex
	orders map[uuid.UUID]*order
	seq    uint64
}

func newBook(symbol string, kind IndexKind) *Book {
	return &Book{
		Symbol: symbol,
		bids:   newIndex(kind, Buy),
		asks:   newIndex(kind, Sell),
		orders: map[uuid.UUID]*order{},
	}
}

func (b *Book) side(s Side) levelIndex {
	if s == Buy {
		return b.bids
	}
	return b.asks
}

// Seq returns the last sequence number emitted for this symbol.
func (b *Book) Seq() uint64 { return b.seq }

// OrderCount returns the number of resting orders.
func (b *Book) OrderCount() int { return len(b.orders) }

// BestBid returns the best bid price, or 0 and false if there are no bids.
func (b *Book) BestBid() (int64, bool) {
	if l := b.bids.best(); l != nil {
		return l.price, true
	}
	return 0, false
}

// BestAsk returns the best ask price, or 0 and false if there are no asks.
func (b *Book) BestAsk() (int64, bool) {
	if l := b.asks.best(); l != nil {
		return l.price, true
	}
	return 0, false
}

// Depth returns up to n aggregated levels per side, best first.
// n <= 0 means all levels.
func (b *Book) Depth(n int) (bids, asks []Level) {
	collect := func(idx levelIndex) []Level {
		out := make([]Level, 0, min(max(n, 0), idx.len()))
		idx.ascend(func(l *level) bool {
			out = append(out, Level{Price: l.price, Qty: l.total, Orders: l.count})
			return n <= 0 || len(out) < n
		})
		return out
	}
	return collect(b.bids), collect(b.asks)
}

func (b *Book) rest(o *order) {
	idx := b.side(o.side)
	l := idx.get(o.price)
	if l == nil {
		l = &level{price: o.price}
		idx.add(l)
	}
	l.push(o)
	b.orders[o.id] = o
}

// unlink removes a resting order from its level (and the level if empty).
func (b *Book) unlink(o *order) {
	l := o.lvl
	l.unlink(o)
	delete(b.orders, o.id)
	if l.count == 0 {
		b.side(o.side).remove(l)
	}
}
