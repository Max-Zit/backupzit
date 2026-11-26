package mysqldb

import "testing"

// B10: binary logs keep their order past mysql-bin.999999.
func TestBinlogOrder(t *testing.T) {
	if !binlogLess("mysql-bin.999999", "mysql-bin.1000000") || binlogLess("mysql-bin.1000000", "mysql-bin.999999") {
		t.Error("999999 before 1000000")
	}
	if !binlogLess("mysql-bin.000009", "mysql-bin.000010") || binlogLess("mysql-bin.000010", "mysql-bin.000010") {
		t.Error("plain order")
	}
	if got := nextBinlog("mysql-bin.999999"); got != "mysql-bin.1000000" {
		t.Errorf("next after 999999: %s", got)
	}
}
