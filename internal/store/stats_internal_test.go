package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

var (
	errStatsQuery     = errors.New("stats query failure")
	errStatsRowsClose = errors.New("stats rows close failure")
)

type statsHook struct {
	mu     sync.Mutex
	mode   string
	writer *sql.DB
	cancel context.CancelFunc
	fired  bool
}

func (h *statsHook) once() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fired {
		return false
	}
	h.fired = true
	return true
}

type statsConnector struct {
	base driver.Driver
	name string
	hook *statsHook
}

func (c statsConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.name)
	if err != nil {
		return nil, err
	}
	return statsConn{Conn: conn, hook: c.hook}, nil
}

func (c statsConnector) Driver() driver.Driver { return c.base }

type statsConn struct {
	driver.Conn
	hook *statsHook
}

func (c statsConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if !strings.Contains(query, "FILTER (WHERE") || !c.hook.once() {
		return queryer.QueryContext(ctx, query, args)
	}
	switch c.hook.mode {
	case "write":
		if _, err := c.hook.writer.Exec(`INSERT INTO locations VALUES(6,'ok','confirmed')`); err != nil {
			return nil, err
		}
	case "query-error":
		return nil, errStatsQuery
	case "cancel":
		c.hook.cancel()
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if c.hook.mode == "rows-close-error" {
		return statsRows{Rows: rows, closeErr: errStatsRowsClose}, nil
	}
	return rows, nil
}

type statsRows struct {
	driver.Rows
	closeErr error
}

func (r statsRows) Close() error {
	_ = r.Rows.Close()
	return r.closeErr
}

func openStatsHarness(t *testing.T, mode string) (*Store, context.Context) {
	t.Helper()
	dsn := fmt.Sprintf("file:stats-test-%p?mode=memory&cache=shared", t)
	writer, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := writer.Exec(`
CREATE TABLE locations(
 id INTEGER PRIMARY KEY,
 tsa_status TEXT NOT NULL,
 ots_status TEXT NOT NULL
);
INSERT INTO locations VALUES
 (1,'fail','confirmed'),
 (2,'ok','submitted'),
 (3,'ok','pending'),
 (4,'pending','fail'),
 (5,'unknown','unknown');`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	hook := &statsHook{mode: mode, writer: writer, cancel: cancel}
	reader := sql.OpenDB(statsConnector{base: writer.Driver(), name: dsn, hook: hook})
	reader.SetMaxOpenConns(1)
	reader.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = reader.Close(); cancel() })
	return &Store{db: reader}, ctx
}

func TestStatsContextNormalStatuses(t *testing.T) {
	st, ctx := openStatsHarness(t, "normal")
	got, err := st.StatsContext(ctx)
	want := Stats{Total: 5, TsaOK: 2, OtsPending: 2, OtsOK: 1, TsaFail: 1, OtsFail: 1}
	if err != nil || got != want {
		t.Fatalf("stats=%+v err=%v want=%+v", got, err, want)
	}
}

func TestStatsContextWriteBetweenQueriesIsCoherent(t *testing.T) {
	st, ctx := openStatsHarness(t, "write")
	got, err := st.StatsContext(ctx)
	want := Stats{Total: 6, TsaOK: 3, OtsPending: 2, OtsOK: 2, TsaFail: 1, OtsFail: 1}
	if err != nil || got != want {
		t.Fatalf("stats=%+v err=%v want=%+v", got, err, want)
	}
}

func TestStatsContextAggregateFailuresReturnInitialTotal(t *testing.T) {
	for _, mode := range []string{"query-error", "cancel", "rows-close-error"} {
		t.Run(mode, func(t *testing.T) {
			st, ctx := openStatsHarness(t, mode)
			got, err := st.StatsContext(ctx)
			if got != (Stats{Total: 5}) {
				t.Fatalf("stats=%+v want initial total only", got)
			}
			switch mode {
			case "query-error":
				if !errors.Is(err, errStatsQuery) {
					t.Fatalf("err=%v want=%v", err, errStatsQuery)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err=%v want context.Canceled", err)
				}
			case "rows-close-error":
				if !errors.Is(err, errStatsRowsClose) {
					t.Fatalf("err=%v want=%v", err, errStatsRowsClose)
				}
			}
		})
	}
}
