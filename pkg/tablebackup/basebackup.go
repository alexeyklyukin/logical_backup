package tablebackup

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx"
)

// connects to the postgresql instance using replication protocol
func (t *TableBackup) connect() error {
	cfg := t.cfg.Merge(pgx.ConnConfig{
		RuntimeParams:        map[string]string{"replication": "database"},
		PreferSimpleProtocol: true,
	})
	conn, err := pgx.Connect(cfg)

	if err != nil {
		return fmt.Errorf("could not connect: %v", err)
	}

	connInfo, err := t.initPostgresql(conn)
	if err != nil {
		return fmt.Errorf("could not fetch conn info: %v", err)
	}
	conn.ConnInfo = connInfo
	t.conn = conn

	return nil
}

func (t *TableBackup) disconnect() error {
	if t.conn == nil {
		return fmt.Errorf("no open connections")
	}

	return t.conn.Close()
}

func (t *TableBackup) tempSlotName() string {
	return fmt.Sprintf("tempslot_%d", t.conn.PID())
}

func (t *TableBackup) RotateOldDeltas(deltasDir string, lastLSN uint64) error {
	fileList, err := ioutil.ReadDir(deltasDir)
	if err != nil {
		return fmt.Errorf("could not list directory: %v", err)
	}
	for _, v := range fileList {
		filename := v.Name()
		lsnStr := filename
		if strings.Contains(filename, ".") {
			parts := strings.Split(filename, ".")
			lsnStr = parts[0]
		}

		lsn, err := strconv.ParseUint(lsnStr, 16, 64)
		if err != nil {
			return fmt.Errorf("could not parse filename: %v", err)
		}
		if lastLSN == lsn {
			continue // skip current file
		}

		if lsn < t.basebackupLSN {
			filename = fmt.Sprintf("%s/%s", deltasDir, filename)
			if err := os.Remove(filename); err != nil {
				return fmt.Errorf("could not remove %q file: %v", filename, err)
			}
		}
	}

	return nil
}

func (t *TableBackup) lockTable() error {
	if _, err := t.tx.Exec(fmt.Sprintf("LOCK TABLE %s IN ACCESS SHARE MODE", t.Identifier.Sanitize())); err != nil {
		return fmt.Errorf("could not lock the table: %v", err)
	}

	return nil
}

func (t *TableBackup) txBegin() error {
	if t.tx != nil {
		return fmt.Errorf("there is already a transaction in progress")
	}
	if t.conn == nil {
		return fmt.Errorf("no postgresql connection")
	}

	tx, err := t.conn.BeginEx(t.ctx, &pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return fmt.Errorf("could not begin tx: %v", err)
	}

	t.tx = tx

	return nil
}

func (t *TableBackup) txCommit() error {
	if t.tx == nil {
		return fmt.Errorf("no running transaction")
	}

	if t.conn == nil {
		return fmt.Errorf("no open connections")
	}

	if err := t.tx.Commit(); err != nil {
		return err
	}

	t.tx = nil
	return nil
}

func (t *TableBackup) txRollback() error {
	if t.tx == nil {
		return fmt.Errorf("no running transaction")
	}

	if t.conn == nil {
		return fmt.Errorf("no open connections")
	}

	if err := t.tx.Rollback(); err != nil {
		return err
	}

	t.tx = nil
	return nil
}

func (t *TableBackup) copyDump() error {
	if t.tx == nil {
		return fmt.Errorf("no running transaction")
	}
	if t.basebackupLSN == 0 {
		return fmt.Errorf("no consistent point")
	}
	tempFilename := fmt.Sprintf("%s.new", t.bbFilename)
	if _, err := os.Stat(tempFilename); os.IsExist(err) {
		os.Remove(tempFilename)
	}

	fp, err := os.OpenFile(tempFilename, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.ModePerm)
	if err != nil {
		return fmt.Errorf("could not open file: %v", err)
	}
	defer fp.Close()

	if err := t.tx.CopyToWriter(fp, fmt.Sprintf("copy %s to stdout", t.Identifier.Sanitize())); err != nil {
		if err2 := t.txRollback(); err2 != nil {
			os.Remove(tempFilename)
			return fmt.Errorf("could not copy and rollback tx: %v, %v", err2, err)
		}
		os.Remove(tempFilename)
		return fmt.Errorf("could not copy: %v", err)
	}
	if err := os.Rename(tempFilename, t.bbFilename); err != nil {
		return fmt.Errorf("could not move file: %v", err)
	}

	return nil
}

func (t *TableBackup) createTempReplicationSlot() error {
	var createdSlotName, basebackupLSN, snapshotName, plugin sql.NullString

	if t.tx == nil {
		return fmt.Errorf("no running transaction")
	}

	row := t.tx.QueryRow(fmt.Sprintf("CREATE_REPLICATION_SLOT %s TEMPORARY LOGICAL %s USE_SNAPSHOT",
		t.tempSlotName(), "pgoutput"))

	if err := row.Scan(&createdSlotName, &basebackupLSN, &snapshotName, &plugin); err != nil {
		return fmt.Errorf("could not scan: %v", err)
	}

	if !basebackupLSN.Valid {
		return fmt.Errorf("null consistent point")
	}

	lsn, err := pgx.ParseLSN(basebackupLSN.String)
	if err != nil {
		return fmt.Errorf("could not parse LSN: %v", err)
	}

	t.basebackupLSN = lsn

	return nil
}