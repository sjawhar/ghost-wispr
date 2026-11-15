package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// walScratchTable holds the zeros overwriteWriteAheadLog writes; it exists
// only while it runs.
const walScratchTable = "erase_write_ahead_log"

// overwriteWriteAheadLog overwrites every frame of db's write-ahead log, the
// file at walPath, with page images that hold no transcript. Frames copied to
// the database long ago stay in the log until a write reuses their place:
// truncating the log frees their blocks with the words still in them.
//
// A RESTART checkpoint copies every frame into the database and waits until
// no reader uses the log, so the next write starts the log again from its
// beginning. That write puts zeros as large as the log into a scratch table,
// in one frame per page, over every old frame; the table is then dropped,
// its pages zeroed by secure_delete. The caller then truncates the log.
func overwriteWriteAheadLog(db *sql.DB, walPath string) error {
	var busy, frames, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(RESTART)`).Scan(&busy, &frames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint before overwriting the write-ahead log: %w", err)
	}
	if busy != 0 {
		return errors.New("checkpoint before overwriting the write-ahead log: a reader kept it from starting over")
	}
	info, err := os.Stat(walPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat the write-ahead log: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin overwriting the write-ahead log: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS ` + walScratchTable + ` (zeros BLOB)`); err != nil {
		return fmt.Errorf("create the write-ahead log scratch table: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO `+walScratchTable+`(zeros) VALUES(zeroblob(?))`, info.Size()); err != nil {
		return fmt.Errorf("overwrite the write-ahead log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit overwriting the write-ahead log: %w", err)
	}
	if _, err := db.Exec(`DROP TABLE ` + walScratchTable); err != nil {
		return fmt.Errorf("drop the write-ahead log scratch table: %w", err)
	}
	return nil
}

// truncateWriteAheadLog copies the write-ahead log into the database and
// empties it. Tests replace it to read the log as it is before the truncation.
var truncateWriteAheadLog = func(db *sql.DB) error {
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}
