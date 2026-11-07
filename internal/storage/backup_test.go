package storage

import (
	"bufio"
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A re-run of this test binary with dbRoleEnv set plays one process over the
// database at dbPathEnv, for TestAWriteSurvivesACrashAfterAnotherProcessOpensTheDatabase.
const (
	dbRoleEnv = "GHOST_WISPR_TEST_DB_ROLE"
	dbPathEnv = "GHOST_WISPR_TEST_DB_PATH"
)

func TestMain(m *testing.M) {
	switch role := os.Getenv(dbRoleEnv); role {
	case "":
		os.Exit(m.Run())
	case "app":
		os.Exit(runAppProcess(os.Getenv(dbPathEnv)))
	case "other":
		os.Exit(runOtherProcess(os.Getenv(dbPathEnv)))
	default:
		fmt.Fprintf(os.Stderr, "unknown %s %q\n", dbRoleEnv, role)
		os.Exit(2)
	}
}

// runAppProcess opens the database as the app does at startup, then creates
// a session for each id it reads on stdin and writes the id back on stdout
// once the session is committed. It never closes the database: the test
// kills it, as a crash or a power cut would.
func runAppProcess(path string) int {
	store, err := NewSQLiteStore(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("ready")
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		id := in.Text()
		if err := store.CreateSession(id, time.Now().UTC()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(id)
	}
	return 0
}

// runOtherProcess uses the database as another SQLite program does (the
// sqlite3 command, a backup script): it opens it, reads it and closes it.
func runOtherProcess(path string) int {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var sessions int
	if err := db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := db.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// The store backs up the database when it opens it. Taking that backup must
// leave the app's hold on the database as SQLite set it: another SQLite
// process that opens and closes the database while the app runs must not
// take it for its last user and delete the write-ahead log the app's writes
// are in. Every write the app committed survives the app being killed.
func TestAWriteSurvivesACrashAfterAnotherProcessOpensTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	// A database from an earlier run, as on every start but the first.
	earlier, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := earlier.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	app := exec.Command(os.Args[0])
	app.Env = append(os.Environ(), dbRoleEnv+"=app", dbPathEnv+"="+path)
	var appStderr bytes.Buffer
	app.Stderr = &appStderr
	stdin, err := app.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := app.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatalf("start the app process: %v", err)
	}
	killed := false
	kill := func() {
		if killed {
			return
		}
		killed = true
		_ = app.Process.Kill()
		_ = app.Wait()
	}
	t.Cleanup(kill)

	lines := make(chan string)
	go func() {
		defer close(lines)
		out := bufio.NewScanner(stdout)
		for out.Scan() {
			lines <- out.Text()
		}
	}()
	expect := func(want string) {
		t.Helper()
		select {
		case got, ok := <-lines:
			if !ok || got != want {
				kill()
				t.Fatalf("app process said %q (open: %v), want %q; its stderr: %s", got, ok, want, appStderr.String())
			}
		case <-time.After(time.Minute):
			kill()
			t.Fatalf("app process said nothing for a minute, want %q; its stderr: %s", want, appStderr.String())
		}
	}
	write := func(id string) {
		t.Helper()
		if _, err := fmt.Fprintln(stdin, id); err != nil {
			t.Fatalf("ask the app process to write %q: %v", id, err)
		}
		expect(id)
	}

	expect("ready")
	write("written-before-the-other-process")
	other := exec.Command(os.Args[0])
	other.Env = append(os.Environ(), dbRoleEnv+"=other", dbPathEnv+"="+path)
	if out, err := other.CombinedOutput(); err != nil {
		t.Fatalf("other process: %v: %s", err, out)
	}
	write("written-after-the-other-process")
	kill()

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store after the crash: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, id := range []string{"written-before-the-other-process", "written-after-the-other-process"} {
		if _, err := store.GetSession(id); err != nil {
			t.Errorf("session %s, committed before the app was killed, is gone after it: %v", id, err)
		}
	}
}

// The backup holds every committed write, those still in the write-ahead log
// (not yet copied into the database file) included.
func TestTheBackupHoldsWritesStillInTheWriteAheadLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	running, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = running.Close() })
	if err := running.CreateSession("in-the-log", time.Now().UTC()); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A second store over the same database takes the backup while the
	// first holds the session in its write-ahead log.
	second, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	backup, err := sql.Open("sqlite", path+".pre-migrate.bak")
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	t.Cleanup(func() { _ = backup.Close() })
	var sessions int
	if err := backup.QueryRow(`SELECT count(*) FROM sessions WHERE id = 'in-the-log'`).Scan(&sessions); err != nil {
		t.Fatalf("query backup: %v", err)
	}
	if sessions != 1 {
		t.Errorf("backup holds %d copies of the committed session, want 1", sessions)
	}
}

// A backup cut off part way (a crash, a power cut) leaves its unfinished copy
// beside the backup; the next start backs up all the same.
func TestABackupCutOffPartWayDoesNotStopTheNext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost-wispr.db")
	first, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := first.CreateSession("kept", time.Now().UTC()); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	backupPath := path + ".pre-migrate.bak"
	if err := os.WriteFile(backupPath+".part", []byte("cut off"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	backup, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	t.Cleanup(func() { _ = backup.Close() })
	var sessions int
	if err := backup.QueryRow(`SELECT count(*) FROM sessions WHERE id = 'kept'`).Scan(&sessions); err != nil {
		t.Fatalf("query backup: %v", err)
	}
	if sessions != 1 {
		t.Errorf("backup holds %d copies of the session, want 1", sessions)
	}
	if _, err := os.Stat(backupPath + ".part"); !os.IsNotExist(err) {
		t.Errorf("unfinished copy still beside the backup: %v", err)
	}
}
