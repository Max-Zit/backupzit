//go:build !windows

package mssql

// systemAccount: SQL Server backups run on Windows; elsewhere the English
// name is all there is to say.
func systemAccount() string { return defaultSystemAccount }
