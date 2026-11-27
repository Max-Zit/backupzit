package mssql

import "golang.org/x/sys/windows"

// systemAccount is the name of the LocalSystem account (SID S-1-5-18) in
// this Windows' language: "NT AUTHORITY\SYSTEM" in English, for example
// "AUTORIDADE NT\SISTEMA" in Portuguese, where the English name is not
// found (error 15151, #78).
func systemAccount() string {
	sid, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return defaultSystemAccount
	}
	name, domain, _, err := sid.LookupAccount("")
	if err != nil || name == "" {
		return defaultSystemAccount
	}
	if domain != "" {
		return domain + `\` + name
	}
	return name
}
