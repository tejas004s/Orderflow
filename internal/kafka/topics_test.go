package kafka

import (
	"strings"
	"testing"
)

func TestTopicsCatalogue(t *testing.T) {
	seen := map[string]TopicSpec{}
	for _, tp := range Topics() {
		if _, dup := seen[tp.Name]; dup {
			t.Fatalf("duplicate topic %s", tp.Name)
		}
		seen[tp.Name] = tp
	}
	if seen[TopicOrderCommands].Partitions != 6 {
		t.Fatal("orders.commands must have 6 partitions")
	}
	if p := *seen[TopicCandles1m].Configs["cleanup.policy"]; !strings.Contains(p, "compact") {
		t.Fatalf("candles must be compacted, got %s", p)
	}
	for _, name := range []string{TopicOrderCommands, TopicOrderEvents, TopicTrades} {
		dlq, ok := seen[DLQ(name)]
		if !ok || dlq.Partitions != 1 {
			t.Fatalf("missing/invalid DLQ for %s", name)
		}
	}
}
