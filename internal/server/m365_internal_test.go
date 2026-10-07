package server

import "testing"

func TestHasRole(t *testing.T) {
	for _, c := range []struct {
		roles []string
		n     string
		want  bool
	}{
		{[]string{"Calendars.Read.All"}, "Calendars.Read", true},
		{[]string{"Calendars.Read"}, "Calendars.Read", true},
		{[]string{"Calendars.ReadWrite"}, "Calendars.Read", true},
		{[]string{"Files.ReadWrite.All"}, "Files.Read.All", true},
		{[]string{"Files.Read.All"}, "Files.Read.All", true},
		{[]string{"Mail.ReadBasic"}, "Mail.Read", false},
		{[]string{"Calendars.ReadBasic.All"}, "Calendars.Read", false},
		{nil, "Sites.Read.All", false},
	} {
		if got := hasRole(c.roles, c.n); got != c.want {
			t.Errorf("hasRole(%v, %s) = %v", c.roles, c.n, got)
		}
	}
}
