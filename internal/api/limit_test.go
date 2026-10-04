package api

import "testing"

func TestSpeedLimitActive(t *testing.T) {
	day := &SpeedLimit{BytesPerSec: 1, FromHour: 8, ToHour: 18}
	night := &SpeedLimit{BytesPerSec: 1, FromHour: 22, ToHour: 6}
	always := &SpeedLimit{BytesPerSec: 1}
	for _, c := range []struct {
		l    *SpeedLimit
		h    int
		want bool
	}{{day, 7, false}, {day, 8, true}, {day, 17, true}, {day, 18, false}, {night, 23, true}, {night, 3, true}, {night, 6, false}, {night, 12, false},
		{always, 0, true}, {nil, 10, false}, {&SpeedLimit{FromHour: 1, ToHour: 2}, 1, false}} {
		if got := c.l.Active(c.h); got != c.want {
			t.Errorf("%+v at %d = %v", c.l, c.h, got)
		}
	}
}
