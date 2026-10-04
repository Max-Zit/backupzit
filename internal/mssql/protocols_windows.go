package mssql

import (
	// Local connections: shared memory (lpc:) and named pipes (np:).
	_ "github.com/microsoft/go-mssqldb/namedpipe"
	_ "github.com/microsoft/go-mssqldb/sharedmemory"
)
