//go:build chaos

package chaos

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// proc is one Loka binary the harness starts, kills and restarts. Each start
// writes to its own log file, so a crash and its restart stay readable.
type proc struct {
	name   string
	bin    string
	env    []string
	logDir string
	ready  func() (bool, error) // reports whether this start is serving

	cmd    *exec.Cmd
	done   chan struct{}
	starts int
}

func (p *proc) logPath() string {
	return filepath.Join(p.logDir, fmt.Sprintf("%s-%d.log", p.name, p.starts))
}

// start launches the binary and waits until it is ready.
func (p *proc) start(t *testing.T) {
	t.Helper()
	p.starts++
	f, err := os.Create(p.logPath())
	if err != nil {
		t.Fatalf("%s: create log: %v", p.name, err)
	}

	cmd := exec.Command(p.bin)
	cmd.Env = append(os.Environ(), p.env...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Fatalf("%s: start: %v", p.name, err)
	}
	p.cmd = cmd
	p.done = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		f.Close()
		close(p.done)
	}()

	waitFor(t, 30*time.Second, p.name+" ready", func() (bool, error) {
		select {
		case <-p.done:
			return false, fmt.Errorf("%s exited during startup; see %s", p.name, p.logPath())
		default:
		}
		return p.ready()
	})
}

// kill simulates a crash. SIGKILL gives the process no chance to finish a
// batch, commit offsets or close connections, which is the point: a graceful
// SIGTERM would only test the happy shutdown path.
func (p *proc) kill(t *testing.T) {
	t.Helper()
	if !p.running() {
		t.Fatalf("%s: kill: not running", p.name)
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("%s: kill: %v", p.name, err)
	}
	<-p.done
}

// stop shuts the process down gracefully, for the end of the run.
func (p *proc) stop() {
	if !p.running() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (p *proc) running() bool {
	if p.cmd == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// waitFor polls cond until it reports true, fails, or timeout passes. The
// harness waits for states, never for fixed times: a slow machine then makes
// the run longer, not flaky.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil {
			t.Fatalf("waiting for %s: %v", what, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s: still not true after %s", what, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// httpAnswers is a readiness check for servers: any HTTP status means the
// port is open and serving.
func httpAnswers(url string) func() (bool, error) {
	client := &http.Client{Timeout: time.Second}
	return func() (bool, error) {
		resp, err := client.Get(url)
		if err != nil {
			return false, nil // not listening yet
		}
		resp.Body.Close()
		return true, nil
	}
}

// httpOK is a readiness check that also needs a 200, e.g. the API's /readyz,
// which pings the database.
func httpOK(url string) func() (bool, error) {
	client := &http.Client{Timeout: time.Second}
	return func() (bool, error) {
		resp, err := client.Get(url)
		if err != nil {
			return false, nil
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	}
}

// logContains is a readiness check for workers with no HTTP port: they are
// ready once this start's log has the startup line.
func logContains(p *proc, msg string) func() (bool, error) {
	needle := fmt.Sprintf(`"msg":%q`, msg)
	return func() (bool, error) {
		f, err := os.Open(p.logPath())
		if err != nil {
			return false, nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.Contains(sc.Text(), needle) {
				return true, nil
			}
		}
		return false, sc.Err()
	}
}

// buildBinaries compiles each command once, so a restart is instant and every
// start runs the same code.
func buildBinaries(t *testing.T, dir string, names ...string) map[string]string {
	t.Helper()
	bins := make(map[string]string, len(names))
	for _, name := range names {
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", out, "./cmd/"+name)
		cmd.Dir = repoRoot()
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, b)
		}
		bins[name] = out
	}
	return bins
}

// createTopic makes a fresh topic for this run. Outbox ids restart at 1 after
// the reset, so reusing a topic would hand the consumer old events whose ids
// collide with new ones, and the dedup table would silently skip the new ones.
func createTopic(t *testing.T, topic string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "exec", "-T", "kafka",
		"/opt/kafka/bin/kafka-topics.sh", "--bootstrap-server", "localhost:9092",
		"--create", "--topic", topic, "--partitions", "3", "--replication-factor", "1")
	cmd.Dir = repoRoot()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create topic %s: %v\n%s", topic, err, b)
	}
}

// resetDB empties every table and creates this run's customer and units. It
// refuses to run against a schema older than the expiry migration.
func resetDB(t *testing.T, pool *pgxpool.Pool) []uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var version int
	if err := pool.QueryRow(ctx, `SELECT version FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v (run make migrate-up)", err)
	}
	if version < 12 {
		t.Fatalf("schema version %d, want >= 12 (run make migrate-up)", version)
	}

	if _, err := pool.Exec(ctx, `
		TRUNCATE outbox_events, processed_events, idempotency_keys, notifications,
		         payments, bookings, inventory_units, customers
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO customers (customer_id, name, email, phone)
		VALUES ($1, 'Chaos Customer', 'chaos@example.com', '+620000000000')`, customerID); err != nil {
		t.Fatalf("insert customer: %v", err)
	}

	units := make([]uuid.UUID, unitCount)
	for i := range units {
		units[i] = uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO inventory_units (unit_id, name, description, available_units, total_units,
			                             currency, price_minor, min_book)
			VALUES ($1, $2, 'chaos test unit', $3, $3, 'IDR', 100000, 1)`,
			units[i], fmt.Sprintf("Chaos Unit %02d", i+1), seatsPerUnit); err != nil {
			t.Fatalf("insert unit: %v", err)
		}
	}
	return units
}

// repoRoot resolves the repository root from this file's location
// (internal/chaos), like testdb does.
func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}
