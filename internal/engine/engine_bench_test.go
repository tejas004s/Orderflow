package engine

import (
	"encoding/binary"
	"math/rand/v2"
	"testing"

	"github.com/google/uuid"
)

func makeWorkload(n int) []Command {
	rng := rand.New(rand.NewPCG(7, 13))
	users := [8]uuid.UUID{
		uid("u0"), uid("u1"), uid("u2"), uid("u3"),
		uid("u4"), uid("u5"), uid("u6"), uid("u7"),
	}
	cmds := make([]Command, n)
	var resting []struct {
		id   uuid.UUID
		user uuid.UUID
	}
	for i := 0; i < n; i++ {
		var cid [16]byte
		binary.BigEndian.PutUint64(cid[8:], uint64(i+1))
		ts := int64(i+1) * 1000
		r := rng.IntN(100)
		if r < 20 && len(resting) > 0 {
			idx := rng.IntN(len(resting))
			target := resting[idx]
			resting[idx] = resting[len(resting)-1]
			resting = resting[:len(resting)-1]
			cmds[i] = Command{
				ID: uuid.UUID(cid), Kind: CmdCancel, Symbol: "AAPL",
				OrderID: target.id, UserID: target.user, Timestamp: ts,
			}
			continue
		}
		var oid [16]byte
		binary.BigEndian.PutUint64(oid[:8], 2)
		binary.BigEndian.PutUint64(oid[8:], uint64(i+1))
		user := users[rng.IntN(len(users))]
		side := Buy
		if rng.IntN(2) == 1 {
			side = Sell
		}
		otype := Limit
		px := int64(9900 + rng.IntN(201))
		if r >= 92 {
			otype = Market
			px = 0
		}
		qty := int64(rng.IntN(40) + 1)
		if otype == Limit {
			resting = append(resting, struct {
				id   uuid.UUID
				user uuid.UUID
			}{uuid.UUID(oid), user})
		}
		cmds[i] = Command{
			ID: uuid.UUID(cid), Kind: CmdPlace, Symbol: "AAPL",
			OrderID: uuid.UUID(oid), UserID: user, Side: side, Type: otype,
			Price: px, Qty: qty, Timestamp: ts,
		}
	}
	return cmds
}

func benchmarkEngine(b *testing.B, kind IndexKind, depthEvery int) {
	const batch = 65536
	cmds := makeWorkload(batch)
	e := New(WithIndex(kind), WithDedupeWindow(1_000_000))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := cmds[i%batch]
		// Give each iteration a unique order ID and command ID so dedupe doesn't short-circuit
		binary.BigEndian.PutUint64(c.ID[0:8], uint64(i+1))
		if c.Kind == CmdPlace {
			binary.BigEndian.PutUint64(c.OrderID[0:8], uint64(i+1))
		}
		c.Timestamp = int64(i+1) * 1000
		e.Process(c)
		if depthEvery > 0 && i%depthEvery == 0 {
			if bk := e.Book("AAPL"); bk != nil {
				bk.Depth(20)
			}
		}
	}
}

func BenchmarkEngineBTree(b *testing.B)          { benchmarkEngine(b, IndexBTree, 0) }
func BenchmarkEngineHeap(b *testing.B)           { benchmarkEngine(b, IndexHeap, 0) }
func BenchmarkEngineBTreeWithDepth(b *testing.B) { benchmarkEngine(b, IndexBTree, 50) }
func BenchmarkEngineHeapWithDepth(b *testing.B)  { benchmarkEngine(b, IndexHeap, 50) }
