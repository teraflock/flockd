package governor

import (
	"testing"
	"time"
)

func at(h, m int) time.Time {
	return time.Date(2026, 1, 1, h, m, 0, 0, time.UTC)
}

func TestParseWindow(t *testing.T) {
	w, err := ParseWindow("22:00-08:00")
	if err != nil {
		t.Fatal(err)
	}
	if w.StartMin != 22*60 || w.EndMin != 8*60 {
		t.Errorf("window = %+v", w)
	}
	for _, bad := range []string{"", "22:00", "25:00-08:00", "22:00-8pm"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) should fail", bad)
		}
	}
}

func TestWindowContains(t *testing.T) {
	day, _ := ParseWindow("09:00-17:00")
	night, _ := ParseWindow("22:00-08:00")

	cases := []struct {
		w    Window
		t    time.Time
		want bool
	}{
		{day, at(9, 0), true},
		{day, at(16, 59), true},
		{day, at(17, 0), false},
		{day, at(8, 59), false},
		{night, at(22, 0), true},
		{night, at(23, 59), true},
		{night, at(0, 0), true},
		{night, at(7, 59), true},
		{night, at(8, 0), false},
		{night, at(12, 0), false},
	}
	for _, c := range cases {
		if got := c.w.Contains(c.t); got != c.want {
			t.Errorf("window %+v contains %s = %v, want %v", c.w, c.t.Format("15:04"), got, c.want)
		}
	}
}
