package relay

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/metric"

	"github.com/nicholaswijaya004/loka/internal/storage"
)

const scope = "github.com/nicholaswijaya004/loka/internal/relay"

// Option configures a Relay.
type Option func(*Relay)

// WithMeterProvider records the relay's metrics there instead of in the
// global provider. Tests use it to read them back.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(r *Relay) { r.meterProvider = mp }
}

// metrics are the relay's traffic, errors and latency. Its saturation, the
// outbox backlog, lives in Postgres: see RegisterBacklogMetrics.
type metrics struct {
	published metric.Int64Counter
	failed    metric.Int64Counter
	batch     metric.Float64Histogram
}

func newMetrics(mp metric.MeterProvider) (*metrics, error) {
	m := mp.Meter(scope)
	published, err1 := m.Int64Counter("loka.relay.events.published",
		metric.WithUnit("{event}"),
		metric.WithDescription("Outbox events marked published, counted once their batch commits."))
	failed, err2 := m.Int64Counter("loka.relay.events.failed",
		metric.WithUnit("{event}"),
		metric.WithDescription("Outbox events Kafka did not acknowledge; each stays unpublished and is retried."))
	batch, err3 := m.Float64Histogram("loka.relay.batch.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Time to fetch, publish and mark one non-empty batch, commit included."),
		// The default buckets (0, 5, 10 ... 10000) assume milliseconds; in
		// seconds every batch would land in the first one. A 100-event
		// batch took about 20 ms on Day 26. A batch Kafka never answers
		// ends at the 10 s delivery timeout, just over 10: 15 and 30 keep
		// those out of +Inf, where histogram_quantile can only say "10".
		metric.WithExplicitBucketBoundaries(0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 30))
	return &metrics{published: published, failed: failed, batch: batch}, errors.Join(err1, err2, err3)
}

// RegisterBacklogMetrics reports the outbox backlog: the number of
// unpublished events and the age of the oldest, read from Postgres each time
// metrics are collected (each Prometheus scrape). Every relay replica would
// report the same table, so aggregate them with max, not sum.
func RegisterBacklogMetrics(store *storage.Store, mp metric.MeterProvider) error {
	m := mp.Meter(scope)
	unpublished, err1 := m.Int64ObservableGauge("loka.outbox.unpublished",
		metric.WithUnit("{event}"),
		metric.WithDescription("Outbox events not yet published."))
	oldest, err2 := m.Float64ObservableGauge("loka.outbox.oldest_unpublished.age",
		metric.WithUnit("s"),
		metric.WithDescription("How long the oldest unpublished outbox event has waited."))
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	_, err := m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		count, age, err := store.OutboxBacklog(ctx)
		if err != nil {
			return err // this collection skips both gauges
		}
		o.ObserveInt64(unpublished, count)
		o.ObserveFloat64(oldest, age.Seconds())
		return nil
	}, unpublished, oldest)
	return err
}
