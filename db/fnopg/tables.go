// Copyright (c) 2018-2019, The Fonero developers
// Copyright (c) 2017, The fnodata developers
// See LICENSE for details.

package fnopg

import (
	"database/sql"
	"fmt"

	"github.com/fonero-project/fnodata/db/dbtypes"
	"github.com/fonero-project/fnodata/db/fnopg/internal"
)

var createTableStatements = map[string]string{
	"meta":           internal.CreateMetaTable,
	"blocks":         internal.CreateBlockTable,
	"transactions":   internal.CreateTransactionTable,
	"vins":           internal.CreateVinTable,
	"vouts":          internal.CreateVoutTable,
	"block_chain":    internal.CreateBlockPrevNextTable,
	"addresses":      internal.CreateAddressTable,
	"tickets":        internal.CreateTicketsTable,
	"votes":          internal.CreateVotesTable,
	"misses":         internal.CreateMissesTable,
	"agendas":        internal.CreateAgendasTable,
	"agenda_votes":   internal.CreateAgendaVotesTable,
	"testing":        internal.CreateTestingTable,
	"proposals":      internal.CreateProposalsTable,
	"proposal_votes": internal.CreateProposalVotesTable,
}

var createTypeStatements = map[string]string{
	"vin_t":  internal.CreateVinType,
	"vout_t": internal.CreateVoutType,
}

// dropDuplicatesInfo defines a minimalistic structure that can be used to
// append information needed to delete duplicates in a given table.
type dropDuplicatesInfo struct {
	TableName    string
	DropDupsFunc func() (int64, error)
}

// TableExists checks if the specified table exists.
func TableExists(db *sql.DB, tableName string) (bool, error) {
	rows, err := db.Query(`select relname from pg_class where relname = $1`,
		tableName)
	if err != nil {
		return false, err
	}

	defer func() {
		if e := rows.Close(); e != nil {
			log.Errorf("Close of Query failed: %v", e)
		}
	}()
	return rows.Next(), nil
}

func dropTable(db *sql.DB, tableName string) error {
	_, err := db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s;`, tableName))
	return err
}

// DropTables drops all of the tables internally recognized tables.
func DropTables(db *sql.DB) {
	for tableName := range createTableStatements {
		log.Infof("DROPPING the \"%s\" table.", tableName)
		if err := dropTable(db, tableName); err != nil {
			log.Errorf(`DROP TABLE "%s" failed.`, tableName)
		}
	}

	_, err := db.Exec(`DROP TYPE IF EXISTS vin;`)
	if err != nil {
		log.Errorf("DROP TYPE vin failed.")
	}
}

// DropTestingTable drops only the "testing" table.
func DropTestingTable(db *sql.DB) error {
	_, err := db.Exec(`DROP TABLE IF EXISTS testing;`)
	return err
}

// AnalyzeAllTables performs an ANALYZE on all tables after setting
// default_statistics_target for the transaction.
func AnalyzeAllTables(db *sql.DB, statisticsTarget int) error {
	dbTx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transactions: %v", err)
	}

	_, err = dbTx.Exec(fmt.Sprintf("SET LOCAL default_statistics_target TO %d;", statisticsTarget))
	if err != nil {
		_ = dbTx.Rollback()
		return fmt.Errorf("failed to set default_statistics_target: %v", err)
	}

	_, err = dbTx.Exec(`ANALYZE;`)
	if err != nil {
		_ = dbTx.Rollback()
		return fmt.Errorf("failed to ANALYZE all tables: %v", err)
	}

	return dbTx.Commit()
}

// AnalyzeTable performs an ANALYZE on the specified table after setting
// default_statistics_target for the transaction.
func AnalyzeTable(db *sql.DB, table string, statisticsTarget int) error {
	dbTx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transactions: %v", err)
	}

	_, err = dbTx.Exec(fmt.Sprintf("SET LOCAL default_statistics_target TO %d;", statisticsTarget))
	if err != nil {
		_ = dbTx.Rollback()
		return fmt.Errorf("failed to set default_statistics_target: %v", err)
	}

	_, err = dbTx.Exec(fmt.Sprintf(`ANALYZE %s;`, table))
	if err != nil {
		_ = dbTx.Rollback()
		return fmt.Errorf("failed to ANALYZE all tables: %v", err)
	}

	return dbTx.Commit()
}

func CreateTypes(db *sql.DB) error {
	var err error
	for typeName, createCommand := range createTypeStatements {
		var exists bool
		exists, err = TypeExists(db, typeName)
		if err != nil {
			return err
		}

		if !exists {
			log.Infof("Creating the \"%s\" type.", typeName)
			_, err = db.Exec(createCommand)
			if err != nil {
				return err
			}
		} else {
			log.Tracef("Type \"%s\" exist.", typeName)
		}
	}
	return err
}

func TypeExists(db *sql.DB, tableName string) (bool, error) {
	rows, err := db.Query(`select typname from pg_type where typname = $1`,
		tableName)
	if err == nil {
		defer func() {
			if e := rows.Close(); e != nil {
				log.Errorf("Close of Query failed: %v", e)
			}
		}()
		return rows.Next(), nil
	}
	return false, err
}

func ClearTestingTable(db *sql.DB) error {
	// Clear the scratch table and reset the serial value.
	_, err := db.Exec(`TRUNCATE TABLE testing;`)
	if err == nil {
		_, err = db.Exec(`SELECT setval('testing_id_seq', 1, false);`)
	}
	return err
}

// CreateTables creates all tables required by fnodata if they do not already
// exist.
func CreateTables(db *sql.DB) error {
	// Create all of the data tables.
	for tableName, createCommand := range createTableStatements {
		err := createTable(db, tableName, createCommand)
		if err != nil {
			return err
		}
	}

	return ClearTestingTable(db)
}

// CreateTable creates one of the known tables by name.
func CreateTable(db *sql.DB, tableName string) error {
	createCommand, tableNameFound := createTableStatements[tableName]
	if !tableNameFound {
		return fmt.Errorf("table name %s unknown", tableName)
	}

	return createTable(db, tableName, createCommand)
}

// createTable creates a table with the given name using the provided SQL
// statement, if it does not already exist.
func createTable(db *sql.DB, tableName, stmt string) error {
	exists, err := TableExists(db, tableName)
	if err != nil {
		return err
	}

	if !exists {
		log.Infof(`Creating the "%s" table.`, tableName)
		_, err = db.Exec(stmt)
		if err != nil {
			return err
		}
	} else {
		log.Tracef(`Table "%s" exists.`, tableName)
	}

	return err
}

// CheckColumnDataType gets the data type of specified table column .
func CheckColumnDataType(db *sql.DB, table, column string) (dataType string, err error) {
	err = db.QueryRow(`SELECT data_type
		FROM information_schema.columns
		WHERE table_name=$1 AND column_name=$2`,
		table, column).Scan(&dataType)
	return
}

// DeleteDuplicates attempts to delete "duplicate" rows in tables where unique
// indexes are to be created.
func (pgb *ChainDB) DeleteDuplicates(barLoad chan *dbtypes.ProgressBarLoad) error {
	allDuplicates := []dropDuplicatesInfo{
		// Remove duplicate vins
		{TableName: "vins", DropDupsFunc: pgb.DeleteDuplicateVins},

		// Remove duplicate vouts
		{TableName: "vouts", DropDupsFunc: pgb.DeleteDuplicateVouts},

		// Remove duplicate transactions
		{TableName: "transactions", DropDupsFunc: pgb.DeleteDuplicateTxns},

		// Remove duplicate agendas
		{TableName: "agendas", DropDupsFunc: pgb.DeleteDuplicateAgendas},

		// Remove duplicate agenda_votes
		{TableName: "agenda_votes", DropDupsFunc: pgb.DeleteDuplicateAgendaVotes},
	}

	var err error
	for _, val := range allDuplicates {
		msg := fmt.Sprintf("Finding and removing duplicate %s entries...", val.TableName)
		if barLoad != nil {
			barLoad <- &dbtypes.ProgressBarLoad{BarID: dbtypes.InitialDBLoad, Subtitle: msg}
		}
		log.Info(msg)

		var numRemoved int64
		if numRemoved, err = val.DropDupsFunc(); err != nil {
			return fmt.Errorf("delete %s duplicate failed: %v", val.TableName, err)
		}

		msg = fmt.Sprintf("Removed %d duplicate %s entries.", numRemoved, val.TableName)
		if barLoad != nil {
			barLoad <- &dbtypes.ProgressBarLoad{BarID: dbtypes.InitialDBLoad, Subtitle: msg}
		}
		log.Info(msg)
	}
	// Signal task is done
	if barLoad != nil {
		barLoad <- &dbtypes.ProgressBarLoad{BarID: dbtypes.InitialDBLoad, Subtitle: " "}
	}
	return nil
}

func (pgb *ChainDB) DeleteDuplicatesRecovery(barLoad chan *dbtypes.ProgressBarLoad) error {
	allDuplicates := []dropDuplicatesInfo{
		// Remove duplicate vins
		{TableName: "vins", DropDupsFunc: pgb.DeleteDuplicateVins},

		// Remove duplicate vouts
		{TableName: "vouts", DropDupsFunc: pgb.DeleteDuplicateVouts},

		// Remove duplicate transactions
		{TableName: "transactions", DropDupsFunc: pgb.DeleteDuplicateTxns},

		// Remove duplicate tickets
		{TableName: "tickets", DropDupsFunc: pgb.DeleteDuplicateTickets},

		// Remove duplicate votes
		{TableName: "votes", DropDupsFunc: pgb.DeleteDuplicateVotes},

		// Remove duplicate misses
		{TableName: "misses", DropDupsFunc: pgb.DeleteDuplicateMisses},

		// Remove duplicate agendas
		{TableName: "agendas", DropDupsFunc: pgb.DeleteDuplicateAgendas},

		// Remove duplicate agenda_votes
		{TableName: "agenda_votes", DropDupsFunc: pgb.DeleteDuplicateAgendaVotes},
	}

	var err error
	for _, val := range allDuplicates {
		msg := fmt.Sprintf("Finding and removing duplicate %s entries...", val.TableName)
		if barLoad != nil {
			barLoad <- &dbtypes.ProgressBarLoad{BarID: dbtypes.InitialDBLoad, Subtitle: msg}
		}
		log.Info(msg)

		var numRemoved int64
		if numRemoved, err = val.DropDupsFunc(); err != nil {
			return fmt.Errorf("delete %s duplicate failed: %v", val.TableName, err)
		}

		msg = fmt.Sprintf("Removed %d duplicate %s entries.", numRemoved, val.TableName)
		if barLoad != nil {
			barLoad <- &dbtypes.ProgressBarLoad{BarID: dbtypes.InitialDBLoad, Subtitle: msg}
		}
		log.Info(msg)
	}
	// Signal task is done
	if barLoad != nil {
		barLoad <- &dbtypes.ProgressBarLoad{BarID: dbtypes.InitialDBLoad, Subtitle: " "}
	}
	return nil
}
