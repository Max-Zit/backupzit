package server_test

import "testing"

// Ids beyond int32 (and beyond int64) in list filters must not make the
// console fail: Postgres types a bare "$1=0" parameter as int4 (found by ZAP).
func TestHugeIDFilters(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	if !c.login("admin", "admin-pass-123") {
		t.Fatal("admin login")
	}
	for _, id := range []string{"2147483648", "9223372036854775807", "61864210318551584555732587504523708981508726"} {
		for _, p := range []string{"/calendar?month=2026-08&agent=" + id, "/reports?period=last7&agent=" + id + "&job=" + id,
			"/reports/pdf?period=last7&agent=" + id + "&job=" + id, "/runs?agent=" + id + "&job=" + id,
			"/api/v1/runs?agent_id=" + id + "&job_id=" + id} {
			if code, _, body := c.do("GET", p, nil); code >= 500 {
				t.Errorf("%s: %d %.200s", p, code, body)
			}
		}
	}
}
