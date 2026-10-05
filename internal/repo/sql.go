package repo

import "time"

// SQLBackup describes the SQL Server backup files in a snapshot: native
// full or transaction log backups, one file per database, stored as
// ordinary files in the snapshot's tree.
type SQLBackup struct {
	// Engine is "postgres", "mysql" or "" for Microsoft SQL Server.
	Engine    string        `json:"engine,omitempty"`
	Instance  string        `json:"instance"` // "" for the default instance
	Kind      string        `json:"kind"`     // "full" or "log"
	Databases []SQLDatabase `json:"databases"`
}

// SQLDatabase is the backup of one database.
type SQLDatabase struct {
	Name     string `json:"name"`
	File     string `json:"file"` // path of the backup file in the snapshot
	Recovery string `json:"recovery,omitempty"`
	Size     uint64 `json:"size"` // backup file size
	// LSNs and times of the backup set (from msdb.dbo.backupset).
	FirstLSN      string    `json:"first_lsn"`
	LastLSN       string    `json:"last_lsn"`
	CheckpointLSN string    `json:"checkpoint_lsn,omitempty"`
	DatabaseLSN   string    `json:"database_backup_lsn,omitempty"`
	Start         time.Time `json:"start"`
	Finish        time.Time `json:"finish"`
}
