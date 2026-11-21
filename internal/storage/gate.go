package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/sjawhar/ghost-wispr/internal/stopping"
)

// sqliteDriver is modernc's driver as database/sql registered it: the one
// that functions registered with sqlite.RegisterScalarFunction reach. Opening
// a database handle connects nothing.
var sqliteDriver = func() driver.Driver {
	db, err := sql.Open("sqlite", "")
	if err != nil {
		panic(fmt.Sprintf("storage: the sqlite driver is not registered: %v", err))
	}
	defer func() { _ = db.Close() }()
	return db.Driver()
}()

// storeGate is the one place the store refuses work once it is closing. Every
// statement on the store's connections, and every transaction from its begin
// to its commit or rollback, is counted while it runs (enter, leave). Close
// shuts the gate: from then on a statement fails with stopping.ErrStopped
// without reaching SQLite, as does a new connection, and Close waits for
// those under way before it closes the store's connection. A statement
// waiting for the store's one connection fails so once it gets it. So nothing
// writes the store after it closed, whichever caller forgot to stop first.
type storeGate struct {
	mu       sync.Mutex
	shut     bool
	inFlight int
	idle     chan struct{} // closed once inFlight reaches 0 after shut
}

// errStoreClosed is the error of a statement or connection the shut gate
// refuses.
var errStoreClosed = fmt.Errorf("storage: the store is closed: %w", stopping.ErrStopped)

// enter counts one statement or transaction, unless the gate is shut.
func (g *storeGate) enter() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut {
		return errStoreClosed
	}
	g.inFlight++
	return nil
}

// refuseIfShut refuses a new connection once the gate is shut.
func (g *storeGate) refuseIfShut() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut {
		return errStoreClosed
	}
	return nil
}

func (g *storeGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight--
	if g.shut && g.inFlight == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

// close shuts the gate and waits, within ctx, for the statements and
// transactions under way. It reports ctx's error when they outlast it, and
// they still run.
func (g *storeGate) close(ctx context.Context) error {
	g.mu.Lock()
	g.shut = true
	if g.inFlight == 0 {
		g.mu.Unlock()
		return nil
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	idle, running := g.idle, g.inFlight
	g.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("storage: %d statements still run on the store: %w", running, context.Cause(ctx))
	}
}

// gatedConnector opens the store's connections through modernc's SQLite
// driver, each behind gate.
type gatedConnector struct {
	dsn  string
	gate *storeGate
}

func (c *gatedConnector) Connect(context.Context) (driver.Conn, error) {
	if err := c.gate.refuseIfShut(); err != nil {
		return nil, err
	}
	raw, err := sqliteDriver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &gatedConn{conn: raw.(sqliteConn), gate: c.gate}, nil
}

func (c *gatedConnector) Driver() driver.Driver { return sqliteDriver }

// sqliteConn is what modernc's connection implements, and what database/sql
// uses of it.
type sqliteConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

// gatedConn is one connection of the store, behind its gate. A statement run
// inside an open transaction is counted by the transaction, so a transaction
// the gate admitted runs to its end after the gate shut.
type gatedConn struct {
	conn sqliteConn
	gate *storeGate
	inTx bool // database/sql gives a transaction's connection to it alone
}

// enter counts a statement, unless it runs inside an open transaction.
func (c *gatedConn) enter() (leave func(), err error) {
	if c.inTx {
		return func() {}, nil
	}
	if err := c.gate.enter(); err != nil {
		return nil, err
	}
	return c.gate.leave, nil
}

func (c *gatedConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *gatedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	leave, err := c.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	stmt, err := c.conn.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &gatedStmt{Stmt: stmt, conn: c}, nil
}

func (c *gatedConn) Close() error { return c.conn.Close() }

func (c *gatedConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *gatedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.gate.enter(); err != nil {
		return nil, err
	}
	tx, err := c.conn.BeginTx(ctx, opts)
	if err != nil {
		c.gate.leave()
		return nil, err
	}
	c.inTx = true
	return &gatedTx{tx: tx, conn: c}, nil
}

func (c *gatedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	leave, err := c.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	return c.conn.ExecContext(ctx, query, args)
}

func (c *gatedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	leave, err := c.enter()
	if err != nil {
		return nil, err
	}
	rows, err := c.conn.QueryContext(ctx, query, args)
	if err != nil {
		leave()
		return nil, err
	}
	return &gatedRows{Rows: rows, leave: sync.OnceFunc(leave)}, nil
}

func (c *gatedConn) Ping(ctx context.Context) error { return c.conn.Ping(ctx) }

func (c *gatedConn) ResetSession(ctx context.Context) error { return c.conn.ResetSession(ctx) }

func (c *gatedConn) IsValid() bool { return c.conn.IsValid() }

// gatedTx is a transaction the gate counts from its begin to its end.
type gatedTx struct {
	tx   driver.Tx
	conn *gatedConn
	once sync.Once
}

func (t *gatedTx) end() {
	t.once.Do(func() {
		t.conn.inTx = false
		t.conn.gate.leave()
	})
}

func (t *gatedTx) Commit() error {
	defer t.end()
	return t.tx.Commit()
}

func (t *gatedTx) Rollback() error {
	defer t.end()
	return t.tx.Rollback()
}

// gatedStmt is a prepared statement whose each run the gate counts.
type gatedStmt struct {
	driver.Stmt
	conn *gatedConn
}

func (s *gatedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	leave, err := s.conn.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	return s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (s *gatedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	leave, err := s.conn.enter()
	if err != nil {
		return nil, err
	}
	rows, err := s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		leave()
		return nil, err
	}
	return &gatedRows{Rows: rows, leave: sync.OnceFunc(leave)}, nil
}

// gatedRows is a query's rows, counted until they close.
type gatedRows struct {
	driver.Rows
	leave func()
}

func (r *gatedRows) Close() error {
	defer r.leave()
	return r.Rows.Close()
}
