// Package storeguard is a test instrument: it fails, and records, every write
// to a store from the moment a test says the store is closed, as the app
// closes it once its stop has returned. A write it records is one the app
// would have tried on a closed database. Tests import it; the app does not.
//
// It watches every write through SQLite itself: a TEMP trigger on each table
// of the store's one connection calls a function this package registers,
// which, once the guard is closed, records the write with the stack of the
// goroutine that made it and fails the statement.
//
// It cannot see a write statement that changes no row (a DELETE that matches
// nothing fires no trigger). Such a statement changes nothing, so a closed
// store loses nothing by refusing it; a test about one wraps the store method
// that makes it.
package storeguard

import (
	"database/sql/driver"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"modernc.org/sqlite"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// writeFunction is the SQL function the guard's triggers call.
const writeFunction = "storeguard_write"

var (
	mu     sync.Mutex
	guards = map[string]*Guard{}
	serial int
)

func init() {
	// The function must exist before a store's connection opens: SQLite
	// gives a connection the functions registered when it opened.
	sqlite.MustRegisterScalarFunction(writeFunction, 3, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		token, _ := args[0].(string)
		mu.Lock()
		g := guards[token]
		mu.Unlock()
		if g == nil {
			return nil, nil
		}
		table, _ := args[1].(string)
		op, _ := args[2].(string)
		return nil, g.wrote(table, op)
	})
}

// Guard watches one store's writes.
type Guard struct {
	token    string
	store    *storage.SQLiteStore
	triggers int

	mu     sync.Mutex
	closed bool
	writes []string
}

// New installs a guard on store: a TEMP trigger after each insert, update and
// delete of every table but the full-text index's (a virtual table, and its
// shadow tables, which the triggers of the sessions table keep).
func New(t testing.TB, store *storage.SQLiteStore) *Guard {
	t.Helper()
	mu.Lock()
	serial++
	g := &Guard{token: fmt.Sprintf("guard-%d", serial), store: store}
	guards[g.token] = g
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		delete(guards, g.token)
		mu.Unlock()
	})

	rows, err := store.DB().Query(`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("storeguard: list the store's tables: %v", err)
	}
	var tables, virtual []string
	for rows.Next() {
		var name, sql string
		if err := rows.Scan(&name, &sql); err != nil {
			t.Fatalf("storeguard: list the store's tables: %v", err)
		}
		if strings.HasPrefix(strings.ToUpper(sql), "CREATE VIRTUAL TABLE") {
			virtual = append(virtual, name)
			continue
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("storeguard: list the store's tables: %v", err)
	}
	for _, table := range tables {
		if shadowOf(table, virtual) {
			continue
		}
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := fmt.Sprintf("storeguard_%s_%s_%s", g.token, table, op)
			stmt := fmt.Sprintf(`CREATE TEMP TRIGGER %q AFTER %s ON main.%q BEGIN SELECT %s('%s', '%s', '%s'); END`,
				strings.ReplaceAll(name, "-", "_"), op, table, writeFunction, g.token, table, op)
			if _, err := store.DB().Exec(stmt); err != nil {
				t.Fatalf("storeguard: watch %s on %s: %v", op, table, err)
			}
			g.triggers++
		}
	}
	if g.triggers == 0 {
		t.Fatal("storeguard: UNKNOWN: the store has no table to watch")
	}
	return g
}

func shadowOf(table string, virtual []string) bool {
	for _, v := range virtual {
		if strings.HasPrefix(table, v+"_") {
			return true
		}
	}
	return false
}

func (g *Guard) wrote(table, op string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		return nil
	}
	stack := make([]byte, 8<<10)
	stack = stack[:runtime.Stack(stack, false)]
	g.writes = append(g.writes, fmt.Sprintf("%s on %s, from:\n%s", op, table, stack))
	return fmt.Errorf("storeguard: %s on %s after the store was closed", op, table)
}

// Close marks the store closed, as the app closes it after its stop: every
// write from now on fails and is recorded. It fails t if the guard's triggers
// are gone (a new connection would have none, and the guard would see
// nothing).
func (g *Guard) Close(t testing.TB) {
	t.Helper()
	var installed int
	if err := g.store.DB().QueryRow(`SELECT count(*) FROM sqlite_temp_master WHERE type = 'trigger' AND name LIKE ?`,
		"storeguard_"+strings.ReplaceAll(g.token, "-", "_")+"_%").Scan(&installed); err != nil {
		t.Fatalf("storeguard: count its triggers: %v", err)
	}
	if installed != g.triggers {
		t.Fatalf("storeguard: UNKNOWN: %d of its %d triggers are on the store's connection, so it would not see every write", installed, g.triggers)
	}
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// Writes returns each write tried since Close, with its goroutine's stack.
func (g *Guard) Writes() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.writes...)
}

// Check fails t for every write tried since Close.
func (g *Guard) Check(t testing.TB) {
	t.Helper()
	for _, w := range g.Writes() {
		t.Errorf("a store write after the store was closed: %s", w)
	}
}
