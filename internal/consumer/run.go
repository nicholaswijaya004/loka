package consumer

import (
	"context"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Run consumes from the client until ctx is cancelled. For each record it
// parses, processes (retrying transient failures), and only after the whole
// batch is in the database does it commit the Kafka offsets.
//
// The client must be created with kgo.DisableAutoCommit and
// kgo.BlockRebalanceOnPoll (see cmd/notifier).
func (c *Consumer) Run(ctx context.Context, cl *kgo.Client) error {
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}

		fetches.EachError(func(topic string, partition int32, err error) {
			c.logger.Error("fetch error", "topic", topic, "partition", partition, "error", err)
		})

		var stopped bool
		fetches.EachRecord(func(r *kgo.Record) {
			if stopped {
				return
			}
			if !c.handleRecord(ctx, r) {
				stopped = true // ctx cancelled mid-retry: don't process further
			}
		})

		if stopped {
			// Not committing: everything in this batch is redelivered after
			// restart, and processed_events turns the repeats into skips.
			cl.AllowRebalance()
			return nil
		}

		if err := cl.CommitUncommittedOffsets(ctx); err != nil {
			// The effects are already in the database. If the commit is
			// lost, the batch is redelivered and deduplicated. Safe.
			c.logger.Error("commit offsets", "error", err)
		}
		cl.AllowRebalance()
	}
}

// handleRecord processes one record until it is done: processed, duplicate,
// or poison. It retries anything else. It returns false only if ctx is
// cancelled before the record is done.
func (c *Consumer) handleRecord(ctx context.Context, r *kgo.Record) bool {
	e, err := parseEvent(r)
	if err != nil {
		// Can never succeed: record it loudly and move past it.
		c.logger.Error("skipping poison message", "error", err)
		return true
	}

	for attempt := 0; ; attempt++ {
		isNew, err := c.Process(ctx, e)
		if err == nil {
			if !isNew {
				c.logger.Info("duplicate skipped", "event_id", e.ID, "partition", r.Partition, "offset", r.Offset)
			}
			return true
		}

		c.logger.Warn("process failed, retrying", "event_id", e.ID, "attempt", attempt+1, "error", err)
		if !sleep(ctx, backoff(attempt)) {
			return false
		}
	}
}

func backoff(attempt int) time.Duration {
	d := 100 * time.Millisecond << min(attempt, 6) // 100ms, 200ms, … capped at 6.4s
	return d
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
