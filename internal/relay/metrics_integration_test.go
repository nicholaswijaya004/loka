//go:build integration

package relay

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nicholaswijaya004/loka/internal/storage"
	"github.com/nicholaswijaya004/loka/internal/testdb"
)

// newMeteredRelay returns a relay whose metrics land in reader, isolated
// from the global provider and from other tests.
func newMeteredRelay(pub Publisher, batchSize int) (*Relay, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	r := New(storage.NewStore(testPool), pub, batchSize, time.Second, testLogger, WithMeterProvider(mp))
	return r, reader
}

// collected is one collection's metrics, by instrument name.
type collected map[string]metricdata.Aggregation

func collect(t *testing.T, reader *sdkmetric.ManualReader) collected {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	c := collected{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			c[m.Name] = m.Data
		}
	}
	return c
}

// counter is a counter's total; 0 if it was never added to.
func (c collected) counter(t *testing.T, name string) int64 {
	t.Helper()
	data, ok := c[name]
	if !ok {
		return 0
	}
	sum, ok := data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s: got %T, want an int64 counter", name, data)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

// observations is how many values a histogram has recorded.
func (c collected) observations(t *testing.T, name string) uint64 {
	t.Helper()
	data, ok := c[name]
	if !ok {
		return 0
	}
	h, ok := data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s: got %T, want a float64 histogram", name, data)
	}
	var n uint64
	for _, dp := range h.DataPoints {
		n += dp.Count
	}
	return n
}

func TestRunOnceCountsPublishedAndFailedEvents(t *testing.T) {
	testdb.Reset(t, testPool)
	ids := insertEvents(t, 3)
	r, reader := newMeteredRelay(&fakePublisher{failOn: map[int64]bool{ids[1]: true}}, 10)

	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	c := collect(t, reader)
	if got := c.counter(t, "loka.relay.events.published"); got != 2 {
		t.Errorf("published: got %d, want 2", got)
	}
	if got := c.counter(t, "loka.relay.events.failed"); got != 1 {
		t.Errorf("failed: got %d, want 1", got)
	}
	if got := c.observations(t, "loka.relay.batch.duration"); got != 1 {
		t.Errorf("batch durations recorded: got %d, want 1", got)
	}
}

// A batch that rolls back is fetched and published again by the next run.
// Counting it now would count those events twice.
func TestRunOnceDoesNotCountARolledBackBatch(t *testing.T) {
	testdb.Reset(t, testPool)
	insertEvents(t, 3)
	r, reader := newMeteredRelay(&fakePublisher{shortResults: true}, 10)

	if _, err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce: want an error for a wrong number of results")
	}

	c := collect(t, reader)
	if got := c.counter(t, "loka.relay.events.published"); got != 0 {
		t.Errorf("published: got %d, want 0 for a rolled-back batch", got)
	}
	// It still took time, and slow failing batches are worth seeing.
	if got := c.observations(t, "loka.relay.batch.duration"); got != 1 {
		t.Errorf("batch durations recorded: got %d, want 1", got)
	}
}

// Kafka acknowledged the events, but marking them failed and the batch rolled
// back: the next run sends them again. Counting them now would count them
// twice.
func TestRunOnceDoesNotCountEventsWhoseMarkRolledBack(t *testing.T) {
	testdb.Reset(t, testPool)
	insertEvents(t, 3)
	r, reader := newMeteredRelay(&fakePublisher{}, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.SetAfterPublish(cancel) // published, then the mark fails

	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatal("RunOnce: want an error when marking fails")
	}

	if got := collect(t, reader).counter(t, "loka.relay.events.published"); got != 0 {
		t.Errorf("published: got %d, want 0 until a batch commits", got)
	}
	var unpublished int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&unpublished); err != nil {
		t.Fatalf("count unpublished: %v", err)
	}
	if unpublished != 3 {
		t.Errorf("unpublished after the rollback: got %d, want 3", unpublished)
	}
}

// Polling an empty outbox is not a batch: two a second at near-zero cost,
// they would pull every percentile toward zero.
func TestRunOnceRecordsNoBatchWhenIdle(t *testing.T) {
	testdb.Reset(t, testPool)
	r, reader := newMeteredRelay(&fakePublisher{}, 10)

	for range 3 {
		if _, err := r.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}

	c := collect(t, reader)
	if got := c.observations(t, "loka.relay.batch.duration"); got != 0 {
		t.Errorf("batch durations recorded: got %d, want 0", got)
	}
	if got := c.counter(t, "loka.relay.events.published"); got != 0 {
		t.Errorf("published: got %d, want 0", got)
	}
}

// The batch histogram uses buckets in seconds, from a millisecond to past
// the 10 s Kafka delivery timeout, not the defaults (0, 5, 10 ... 10000).
func TestBatchDurationBucketsAreInSeconds(t *testing.T) {
	testdb.Reset(t, testPool)
	insertEvents(t, 1)
	r, reader := newMeteredRelay(&fakePublisher{}, 10)
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	h, ok := collect(t, reader)["loka.relay.batch.duration"].(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) != 1 {
		t.Fatalf("batch duration: got %+v, want one histogram data point", h)
	}
	bounds := h.DataPoints[0].Bounds
	if len(bounds) == 0 || bounds[0] != 0.001 || bounds[len(bounds)-1] <= 10 {
		t.Errorf("bucket bounds: got %v, want 0.001 s up to past 10 s", bounds)
	}
}

func backlog(t *testing.T, c collected) (count int64, ageSeconds float64, ok bool) {
	t.Helper()
	n, okN := c["loka.outbox.unpublished"].(metricdata.Gauge[int64])
	a, okA := c["loka.outbox.oldest_unpublished.age"].(metricdata.Gauge[float64])
	if !okN || !okA || len(n.DataPoints) != 1 || len(a.DataPoints) != 1 {
		return 0, 0, false
	}
	return n.DataPoints[0].Value, a.DataPoints[0].Value, true
}

func TestBacklogMetricsReadTheOutbox(t *testing.T) {
	testdb.Reset(t, testPool)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	store := storage.NewStore(testPool)
	if err := RegisterBacklogMetrics(store, mp); err != nil {
		t.Fatalf("RegisterBacklogMetrics: %v", err)
	}

	// Empty: both gauges are reported, as zero, not left out.
	count, age, ok := backlog(t, collect(t, reader))
	if !ok || count != 0 || age != 0 {
		t.Errorf("empty outbox: got count %d age %.1f (reported %v), want 0 and 0", count, age, ok)
	}

	// Three waiting, the oldest for 90 seconds.
	ids := insertEvents(t, 3)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE outbox_events SET created_at = now() - interval '90 seconds' WHERE id = $1`, ids[0]); err != nil {
		t.Fatalf("age the first event: %v", err)
	}
	count, age, ok = backlog(t, collect(t, reader))
	if !ok || count != 3 || age < 90 || age > 100 {
		t.Errorf("backlog: got count %d age %.1f s (reported %v), want 3 and about 90 s", count, age, ok)
	}

	// Published events leave the backlog.
	if err := store.MarkOutboxEventsPublished(context.Background(), ids); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	count, age, ok = backlog(t, collect(t, reader))
	if !ok || count != 0 || age != 0 {
		t.Errorf("after publishing: got count %d age %.1f (reported %v), want 0 and 0", count, age, ok)
	}
}
