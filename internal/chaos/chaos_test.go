//go:build chaos

// Package chaos runs Loka's real binaries under load, kills and restarts them,
// lets the system drain, then checks the invariants from ADR-003.
//
// It needs Postgres and Kafka from `make up`, migrations applied, and nothing
// else on ports 8080 and 8081. It wipes the database. Run it with:
//
//	go test -tags=chaos -count=1 -v -timeout=30m ./internal/chaos/
//
// The size of the run comes from the environment (defaults in brackets):
//
//	CHAOS_BOOKINGS  bookings the load sends [1000]
//	CHAOS_RATE      bookings per second     [5]
//
// For example, 10,000 bookings at 25 per second:
//
//	CHAOS_BOOKINGS=10000 CHAOS_RATE=25 go test -tags=chaos -count=1 -v -timeout=60m ./internal/chaos/
package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	dsn        = "postgres://loka:loka@localhost:5432/loka?sslmode=disable"
	brokers    = "localhost:9092"
	apiURL     = "http://localhost:8080"
	paymockURL = "http://localhost:8081"
	customerID = "11111111-1111-1111-1111-111111111111"
)

// The run's shape. totalBookings take about totalBookings / bookingRate
// seconds to send: long enough for the whole chaos schedule to happen while
// traffic is still flowing.
var (
	totalBookings = envInt("CHAOS_BOOKINGS", 1000)
	bookingRate   = envInt("CHAOS_RATE", 5) // per second

	// Concurrency needed ≈ rate × API latency; this leaves ample headroom.
	loadWorkers = max(10, bookingRate)

	// Twice the demand, so no unit sells out: selling out is not what's tested.
	seatsPerUnit = max(100, 2*totalBookings/unitCount)
)

const (
	unitCount = 20

	// Short, so a relay outage expires bookings within the run (M2). Which
	// bookings expire doesn't matter: every invariant holds either way.
	expireAfter = 30 * time.Second

	declineRate      = "0.3"
	lostResponseRate = "0.1"
)

// drainStallTimeout is how long the drain may go without finishing any work
// before the test calls it stuck. It is the slowest path a single booking can
// legitimately take, derived from cmd/payments' config: a charge that keeps
// failing while the breaker is open, whose backoff grows to MaxBackoff.
//
//	charge timeout   10 s   (the call that got no answer)
//	lease            30 s   (always waited in full before a retry)
//	max backoff     300 s   (jitter on top of the lease, capped)
//	breaker open     30 s   (calls refused until the probe)
//	expiry           35 s   (expireAfter + the expirer's 5 s poll)
//	                ------
//	                405 s   ≈ 7 min, rounded up to 8 for scheduling noise
//
// It bounds a stall, not the whole drain: the drain itself is as long as the
// backlog needs, and the go test -timeout caps the run.
const drainStallTimeout = 8 * time.Minute

// One client with a timeout for everything: http.DefaultClient has none, and
// a hung API would block a worker forever.
var httpClient = &http.Client{Timeout: 10 * time.Second}

func TestChaos(t *testing.T) {
	ctx := context.Background()
	t.Logf("run: %d bookings at %d/s, %d units × %d seats",
		totalBookings, bookingRate, unitCount, seatsPerUnit)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("postgres unreachable (run make up): %v", err)
	}

	// --- Step 1: setup -------------------------------------------------------

	units := resetDB(t, pool)
	topic := fmt.Sprintf("booking-events-chaos-%d", time.Now().Unix())
	createTopic(t, topic)

	logDir, err := os.MkdirTemp("", "loka-chaos-")
	if err != nil {
		t.Fatalf("log dir: %v", err)
	}
	t.Logf("process logs: %s", logDir)

	bins := buildBinaries(t, t.TempDir(), "api", "relay", "payments", "paymock")
	db := "DATABASE_URL=" + dsn

	paymock := &proc{name: "paymock", bin: bins["paymock"], logDir: logDir, env: []string{
		"PORT=8081", "DECLINE_RATE=" + declineRate, "LOST_RESPONSE_RATE=" + lostResponseRate,
	}}
	paymock.ready = httpAnswers(paymockURL + "/charges")

	api := &proc{name: "api", bin: bins["api"], logDir: logDir, env: []string{db}}
	api.ready = httpOK(apiURL + "/readyz")

	relay := &proc{name: "relay", bin: bins["relay"], logDir: logDir, env: []string{
		db, "KAFKA_BROKERS=" + brokers, "KAFKA_TOPIC=" + topic,
	}}
	relay.ready = logContains(relay, "relay started")

	payments := &proc{name: "payments", bin: bins["payments"], logDir: logDir, env: []string{
		db, "KAFKA_BROKERS=" + brokers, "KAFKA_TOPIC=" + topic,
		"PAYMENT_PROVIDER_URL=" + paymockURL,
		"BOOKING_EXPIRE_AFTER=" + expireAfter.String(),
	}}
	payments.ready = logContains(payments, "payments started")

	all := []*proc{paymock, api, relay, payments}
	t.Cleanup(func() {
		for _, p := range all {
			p.stop()
		}
	})
	for _, p := range all {
		p.start(t)
	}

	// Smoke check: one booking travels the whole pipeline and ends final.
	// Without it, a wiring mistake would only show up minutes later as
	// "nothing drained".
	smokeID := book(t, units[0])
	waitFor(t, time.Minute, "smoke booking to finish", func() (bool, error) {
		n, err := count(ctx, pool, `
			SELECT count(*) FROM bookings
			WHERE booking_id = $1 AND booking_status IN ('confirmed', 'cancelled')`, smokeID)
		return n == 1, err
	})
	t.Logf("smoke booking %s finished", smokeID)

	// --- Step 2: load ----------------------------------------------------------

	// The load runs on its own goroutine so the chaos schedule can run on this
	// one at the same time. Cancelling stops it if the test fails early.
	loadCtx, cancelLoad := context.WithCancel(ctx)
	t.Cleanup(cancelLoad)
	loadDone := make(chan loadResult, 1) // buffered: the load can finish even if nobody reads
	go func() { loadDone <- runLoad(loadCtx, units) }()

	// --- Step 3: chaos schedule ----------------------------------------------

	// Each fault starts when the load has reached a share of its bookings, not
	// at a clock time, so it always lands while traffic is flowing, whatever
	// the size and rate of the run.
	start := time.Now()
	mark := func(format string, args ...any) {
		t.Logf("[%5.1fs] %s", time.Since(start).Seconds(), fmt.Sprintf(format, args...))
	}
	percentOfLoad := func(p int) int { return totalBookings * p / 100 }

	// 1. Crash the payment worker, consumer and expirer mid-flight. Leases run
	//    out and Kafka redelivers whatever wasn't committed.
	//
	//    A SIGKILLed consumer never leaves its group, so the broker keeps its
	//    partitions until the session timeout (about 45 s) passes. The new
	//    process is up in 5 s but consumes nothing until then, and with a
	//    30 s deadline the bookings waiting in pending expire. Production's
	//    15 min deadline is far above that window.
	waitForBookings(t, pool, percentOfLoad(10))
	mark("kill payments")
	payments.kill(t)
	pause(5 * time.Second)
	payments.start(t)
	mark("payments back")

	// 2. Crash the relay. Events published but not yet marked are published
	//    again: duplicates on Kafka, which the consumer must absorb.
	waitForBookings(t, pool, percentOfLoad(25))
	mark("kill relay")
	relay.kill(t)
	pause(5 * time.Second)
	relay.start(t)
	mark("relay back")

	// 3. A relay outage that lasts until bookings expire. It ends on a state
	//    (5 more expired bookings), not after a fixed time, so it works at any
	//    machine speed. Their booking.created events go out after the restart
	//    and must lose the race to the expirer: no charge. The count starts
	//    from a baseline, because the payments crash in step 1 has usually
	//    expired some bookings already.
	waitForBookings(t, pool, percentOfLoad(40))
	expiredBefore, err := count(ctx, pool, `SELECT count(*) FROM bookings WHERE failure_reason = 'expired'`)
	if err != nil {
		t.Fatalf("count expired: %v", err)
	}
	mark("relay down until 5 more bookings expire (%d so far)", expiredBefore)
	relay.kill(t)
	waitFor(t, 3*time.Minute, "5 more expired bookings", func() (bool, error) {
		n, err := count(ctx, pool, `SELECT count(*) FROM bookings WHERE failure_reason = 'expired'`)
		return n >= expiredBefore+5, err
	})
	relay.start(t)
	mark("relay back")

	// 4. Crash payments again, while lost responses are likely in flight.
	waitForBookings(t, pool, percentOfLoad(75))
	mark("kill payments")
	payments.kill(t)
	pause(5 * time.Second)
	payments.start(t)
	mark("payments back")

	load := <-loadDone
	mark("load finished: %d created, other statuses %v, %d unknown",
		len(load.created), load.statuses, len(load.unknown))

	// --- Step 4: drain ---------------------------------------------------------

	waitForDrain(t, pool, mark)
	mark("drained")

	// --- Step 5: invariants ----------------------------------------------------

	ledger := fetchLedger(t)
	checkDatabaseInvariants(t, pool)
	checkProviderInvariants(t, pool, ledger)
	checkTestValidity(t, pool, ledger, load, smokeID, relay, payments)
	summarize(t, pool, ledger)
}

// --- Load -----------------------------------------------------------------------

// loadResult is what the load generator saw, merged from all workers.
type loadResult struct {
	created  []uuid.UUID // 201s: bookings that certainly exist
	statuses map[int]int // every other HTTP status, counted
	unknown  []error     // transport errors: the booking may or may not exist
}

// runLoad sends totalBookings at bookingRate, round-robin over units. It runs
// off the test goroutine, so it never calls t: it only reports.
func runLoad(ctx context.Context, units []uuid.UUID) loadResult {
	jobs := make(chan uuid.UUID)

	// Producer: one booking per tick, so the load is spread over time.
	go func() {
		defer close(jobs) // ends every worker's range loop
		ticker := time.NewTicker(time.Second / time.Duration(bookingRate))
		defer ticker.Stop()
		for i := range totalBookings {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			select {
			case <-ctx.Done():
				return
			case jobs <- units[i%len(units)]:
			}
		}
	}()

	// Workers: each owns one slot of perWorker, so nothing is shared and no
	// lock is needed.
	perWorker := make([]loadResult, loadWorkers)
	var wg sync.WaitGroup
	for w := range loadWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := loadResult{statuses: map[int]int{}}
			for unitID := range jobs {
				id, status, err := tryBook(ctx, unitID)
				switch {
				case err != nil:
					r.unknown = append(r.unknown, err)
				case status == http.StatusCreated:
					r.created = append(r.created, id)
				default:
					r.statuses[status]++
				}
			}
			perWorker[w] = r
		}()
	}
	wg.Wait()

	merged := loadResult{statuses: map[int]int{}}
	for _, r := range perWorker {
		merged.created = append(merged.created, r.created...)
		merged.unknown = append(merged.unknown, r.unknown...)
		for status, n := range r.statuses {
			merged.statuses[status] += n
		}
	}
	return merged
}

// tryBook creates one booking and reports the outcome instead of failing.
// A transport error means the outcome is unknown: the request may have
// reached the API and the response been lost.
func tryBook(ctx context.Context, unitID uuid.UUID) (uuid.UUID, int, error) {
	body, err := json.Marshal(map[string]any{
		"unit_id":         unitID,
		"customer_id":     customerID,
		"qty":             1,
		"visit_date_time": time.Now().Add(24 * time.Hour).UTC(),
	})
	if err != nil {
		return uuid.Nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/bookings", bytes.NewReader(body))
	if err != nil {
		return uuid.Nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())

	resp, err := httpClient.Do(req)
	if err != nil {
		return uuid.Nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return uuid.Nil, resp.StatusCode, nil
	}
	var out struct {
		BookingID uuid.UUID `json:"booking_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return uuid.Nil, resp.StatusCode, fmt.Errorf("decode: %w", err)
	}
	return out.BookingID, http.StatusCreated, nil
}

// book is tryBook for the test goroutine: anything but a 201 fails the test.
func book(t *testing.T, unitID uuid.UUID) uuid.UUID {
	t.Helper()
	id, status, err := tryBook(context.Background(), unitID)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("book: status %d, err %v", status, err)
	}
	return id
}

// waitForBookings blocks until the load has created at least n bookings.
func waitForBookings(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	waitFor(t, 5*time.Minute, fmt.Sprintf("%d bookings", n), func() (bool, error) {
		got, err := count(context.Background(), pool, `SELECT count(*) FROM bookings`)
		return got >= n, err
	})
}

// pause is how long a crashed process stays down. It's a knob for the
// outage's length, not a guess about when something will be ready.
func pause(d time.Duration) { time.Sleep(d) }

// --- Drain ----------------------------------------------------------------------

// waitForDrain waits until the system has settled: no booking in flight,
// every event published, and every published event processed by payments.
//
// It fails only when the remaining work stops shrinking for
// drainStallTimeout. A fixed deadline would have to be sized in advance for
// the largest backlog the run could build; a stall timeout fits any backlog,
// and still catches a system that is stuck rather than slow.
func waitForDrain(t *testing.T, pool *pgxpool.Pool, mark func(string, ...any)) {
	t.Helper()
	ctx := context.Background()

	best := -1 // the least work seen left so far
	lastProgress := time.Now()
	lastReport := time.Now()
	for {
		// The work left can rise for a moment (recording an outcome adds an
		// event to publish and process), so progress means beating the best
		// so far, not merely going down.
		left, err := count(ctx, pool, `
			SELECT (SELECT count(*) FROM bookings
			        WHERE booking_status IN ('pending', 'payment_pending'))
			     + (SELECT count(*) FROM outbox_events WHERE published_at IS NULL)
			     + (SELECT count(*) FROM outbox_events o
			        WHERE NOT EXISTS (SELECT 1 FROM processed_events p
			                          WHERE p.consumer = 'payments' AND p.event_id = o.id))`)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if left == 0 {
			return
		}
		if best < 0 || left < best {
			best, lastProgress = left, time.Now()
		}
		if time.Since(lastProgress) > drainStallTimeout {
			t.Fatalf("drain stalled: %d units of work left, none finished for %s", left, drainStallTimeout)
		}
		if time.Since(lastReport) >= 30*time.Second {
			mark("draining: %d units of work left", left)
			lastReport = time.Now()
		}
		time.Sleep(time.Second)
	}
}

// --- Invariants ----------------------------------------------------------------

// checkDatabaseInvariants runs the checks that need only Loka's own tables.
// Each query returns one line per violation, so an empty result means the
// invariant holds, and a failure shows the offending rows.
func checkDatabaseInvariants(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	expectNone(t, pool, "S1 available_units >= 0", `
		SELECT format('unit %s: available %s', unit_id, available_units)
		FROM inventory_units WHERE available_units < 0`)

	// After drain only confirmed bookings hold seats, so this is exact.
	expectNone(t, pool, "S2 available = total - confirmed seats", `
		SELECT format('unit %s: available %s, total %s, confirmed seats %s',
		              u.unit_id, u.available_units, u.total_units, coalesce(c.seats, 0))
		FROM inventory_units u
		LEFT JOIN (SELECT unit_id, sum(qty) AS seats FROM bookings
		           WHERE booking_status = 'confirmed' GROUP BY unit_id) c USING (unit_id)
		WHERE u.available_units <> u.total_units - coalesce(c.seats, 0)`)

	expectNone(t, pool, "S3 confirmed has exactly 1 succeeded payment, cancelled has none", `
		SELECT format('booking %s (%s): %s succeeded payments', b.booking_id, b.booking_status, count(p.payment_id))
		FROM bookings b
		LEFT JOIN payments p ON p.booking_id = b.booking_id AND p.payment_status = 'succeeded'
		GROUP BY b.booking_id, b.booking_status
		HAVING (b.booking_status = 'confirmed' AND count(p.payment_id) <> 1)
		    OR (b.booking_status = 'cancelled' AND count(p.payment_id) <> 0)`)

	expectNone(t, pool, "S3b an expired booking has no payment rows at all", `
		SELECT format('booking %s: %s payment rows', b.booking_id, count(*))
		FROM bookings b JOIN payments p ON p.booking_id = b.booking_id
		WHERE b.failure_reason = 'expired'
		GROUP BY b.booking_id`)

	expectNone(t, pool, "S4 at most one succeeded payment per booking", `
		SELECT format('booking %s: %s succeeded payments', booking_id, count(*))
		FROM payments WHERE payment_status = 'succeeded'
		GROUP BY booking_id HAVING count(*) > 1`)

	// S7 plus E2: every booking has exactly one booking.created and exactly
	// one outcome event that matches its final status, never both outcomes.
	expectNone(t, pool, "S7/E2 events match each booking's final status", `
		SELECT format('booking %s (%s): created %s, confirmed %s, cancelled %s',
		              b.booking_id, b.booking_status, e.created, e.confirmed, e.cancelled)
		FROM bookings b
		JOIN LATERAL (
			SELECT count(*) FILTER (WHERE event_type = 'booking.created')   AS created,
			       count(*) FILTER (WHERE event_type = 'booking.confirmed') AS confirmed,
			       count(*) FILTER (WHERE event_type = 'booking.cancelled') AS cancelled
			FROM outbox_events WHERE aggregate_id = b.booking_id
		) e ON true
		WHERE NOT (e.created = 1 AND (
			(b.booking_status = 'confirmed' AND e.confirmed = 1 AND e.cancelled = 0) OR
			(b.booking_status = 'cancelled' AND e.confirmed = 0 AND e.cancelled = 1)))`)

	expectNone(t, pool, "E1 every event published", `
		SELECT format('event %s (%s) unpublished', id, event_type)
		FROM outbox_events WHERE published_at IS NULL`)

	// The payments consumer records every event it receives, of any type, so
	// comparing the two id sets proves both directions.
	expectNone(t, pool, "E4 every published event processed by payments", `
		SELECT format('event %s (%s) never processed', o.id, o.event_type)
		FROM outbox_events o
		WHERE NOT EXISTS (SELECT 1 FROM processed_events p
		                  WHERE p.consumer = 'payments' AND p.event_id = o.id)`)
	expectNone(t, pool, "E3 payments processed no event that isn't in the outbox", `
		SELECT format('processed event %s has no outbox row', p.event_id)
		FROM processed_events p
		WHERE p.consumer = 'payments'
		  AND NOT EXISTS (SELECT 1 FROM outbox_events o WHERE o.id = p.event_id)`)

	expectNone(t, pool, "L1 nothing left in flight", `
		SELECT format('booking %s still %s', booking_id, booking_status)
		FROM bookings WHERE booking_status IN ('pending', 'payment_pending')`)
}

// charge is one entry of paymock's ledger (GET /charges).
type charge struct {
	BookingID     uuid.UUID `json:"booking_id"`
	PaymentStatus string    `json:"payment_status"`
	LostResponse  bool      `json:"lost_response"`
	Calls         int       `json:"calls"`
}

func fetchLedger(t *testing.T) []charge {
	t.Helper()
	resp, err := httpClient.Get(paymockURL + "/charges")
	if err != nil {
		t.Fatalf("fetch ledger: %v", err)
	}
	defer resp.Body.Close()
	var ledger []charge
	if err := json.NewDecoder(resp.Body).Decode(&ledger); err != nil {
		t.Fatalf("decode ledger: %v", err)
	}
	return ledger
}

// checkProviderInvariants compares the provider's records with Loka's. Loka's
// tables could look perfect while a customer was charged twice; only the
// provider's side can show that.
func checkProviderInvariants(t *testing.T, pool *pgxpool.Pool, ledger []charge) {
	t.Helper()

	charged := map[uuid.UUID]int{} // booking -> successful charges at the provider
	declined := map[uuid.UUID]bool{}
	for _, c := range ledger {
		switch c.PaymentStatus {
		case "succeeded":
			charged[c.BookingID]++
		case "declined":
			declined[c.BookingID] = true
		}
	}

	// S5: one successful charge per booking, at most.
	for id, n := range charged {
		if n > 1 {
			t.Errorf("S5: booking %s charged %d times at the provider", id, n)
		}
	}

	confirmed := idSet(t, pool, `SELECT booking_id FROM bookings WHERE booking_status = 'confirmed'`)
	declinedInLoka := idSet(t, pool, `
		SELECT booking_id FROM bookings
		WHERE booking_status = 'cancelled' AND failure_reason IS DISTINCT FROM 'expired'`)

	// S6: charged at the provider ⇔ confirmed in Loka, both directions.
	for id := range charged {
		if !confirmed[id] {
			t.Errorf("S6: booking %s was charged but is not confirmed (money taken, no seat)", id)
		}
	}
	for id := range confirmed {
		if charged[id] == 0 {
			t.Errorf("S6: booking %s is confirmed but was never charged", id)
		}
	}

	// S6b: declined at the provider ⇔ cancelled by a decline in Loka.
	for id := range declined {
		if !declinedInLoka[id] {
			t.Errorf("S6b: booking %s was declined but Loka didn't cancel it for that", id)
		}
	}
	for id := range declinedInLoka {
		if !declined[id] {
			t.Errorf("S6b: booking %s was cancelled as declined but the provider never declined it", id)
		}
	}
}

// checkTestValidity proves the run actually exercised what it claims (M1,
// M2). Without it, an all-green run could mean the chaos never happened.
func checkTestValidity(t *testing.T, pool *pgxpool.Pool, ledger []charge, load loadResult,
	smokeID uuid.UUID, relay, payments *proc) {
	t.Helper()
	ctx := context.Background()

	// M1: every 201 exists, and the database holds no booking the load didn't
	// account for. Unknown outcomes (transport errors) may or may not exist.
	inDB := idSet(t, pool, `SELECT booking_id FROM bookings`)
	for _, id := range append(load.created, smokeID) {
		if !inDB[id] {
			t.Errorf("M1: booking %s got a 201 but isn't in the database", id)
		}
	}
	minWant := len(load.created) + 1 // + the smoke booking
	maxWant := minWant + len(load.unknown)
	if n := len(inDB); n < minWant || n > maxWant {
		t.Errorf("M1: %d bookings in the database, want %d..%d", n, minWant, maxWant)
	}
	for status, n := range load.statuses {
		if status != http.StatusConflict { // 409 = sold out, legitimate
			t.Errorf("M1: the API answered %d %d times", status, n)
		}
	}

	// M2: the chaos really happened.
	var lost, replays, declines int
	for _, c := range ledger {
		if c.LostResponse {
			lost++
		}
		if c.Calls > 1 {
			replays++
		}
		if c.PaymentStatus == "declined" {
			declines++
		}
	}
	expired, err := count(ctx, pool, `SELECT count(*) FROM bookings WHERE failure_reason = 'expired'`)
	if err != nil {
		t.Fatalf("count expired: %v", err)
	}
	confirmed, err := count(ctx, pool, `SELECT count(*) FROM bookings WHERE booking_status = 'confirmed'`)
	if err != nil {
		t.Fatalf("count confirmed: %v", err)
	}
	for name, got := range map[string]int{
		"relay restarts":           relay.starts - 1,
		"payments restarts":        payments.starts - 1,
		"lost responses injected":  lost,
		"charges retried (replay)": replays,
		"declines":                 declines,
		"expired bookings":         expired,
		"confirmed bookings":       confirmed,
	} {
		if got < 1 {
			t.Errorf("M2: %s = %d, want at least 1: the chaos didn't happen", name, got)
		}
	}
}

// summarize logs the run's numbers for the metrics entry.
func summarize(t *testing.T, pool *pgxpool.Pool, ledger []charge) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT booking_status, coalesce(failure_reason, '-'), count(*)
		FROM bookings GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status, reason string
		var n int
		if err := rows.Scan(&status, &reason, &n); err != nil {
			t.Fatalf("summary scan: %v", err)
		}
		t.Logf("bookings %-9s reason %-18s %d", status, reason, n)
	}

	var lost, replays, calls int
	for _, c := range ledger {
		calls += c.Calls
		if c.LostResponse {
			lost++
		}
		if c.Calls > 1 {
			replays++
		}
	}
	t.Logf("provider: %d charges, %d calls, %d lost responses, %d charges retried",
		len(ledger), calls, lost, replays)
}

// --- Helpers --------------------------------------------------------------------

// envInt reads a positive integer from the environment, or returns fallback.
func envInt(key string, fallback int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func count(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) (int, error) {
	var n int
	err := pool.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}

// expectNone fails the test if query returns any rows, showing the first few.
func expectNone(t *testing.T, pool *pgxpool.Pool, invariant, query string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("%s: query: %v", invariant, err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("%s: scan: %v", invariant, err)
		}
		found = append(found, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: rows: %v", invariant, err)
	}
	if len(found) == 0 {
		t.Logf("✓ %s", invariant)
		return
	}
	t.Errorf("✗ %s: %d violations, e.g. %v", invariant, len(found), found[:min(5, len(found))])
}

func idSet(t *testing.T, pool *pgxpool.Pool, query string) map[uuid.UUID]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("query ids: %v", err)
	}
	defer rows.Close()
	set := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		set[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read ids: %v", err)
	}
	return set
}
