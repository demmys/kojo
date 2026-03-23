package agent

import (
	"testing"
	"time"
)

func TestJpHolidayName(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	tests := []struct {
		date string
		want string
	}{
		// Fixed holidays
		{"2026-01-01", "元日"},
		{"2026-02-11", "建国記念の日"},
		{"2026-02-23", "天皇誕生日"},
		{"2026-04-29", "昭和の日"},
		{"2026-05-03", "憲法記念日"},
		{"2026-05-04", "みどりの日"},
		{"2026-05-05", "こどもの日"},
		{"2026-11-03", "文化の日"},
		{"2026-11-23", "勤労感謝の日"},

		// Happy Monday holidays 2026
		{"2026-01-12", "成人の日"},
		{"2026-07-20", "海の日"},
		{"2026-09-21", "敬老の日"},
		{"2026-10-12", "スポーツの日"},

		// Mountain Day
		{"2026-08-11", "山の日"},

		// Equinox days 2026
		{"2026-03-20", "春分の日"},
		{"2026-09-23", "秋分の日"},

		// 振替休日: 2023-01-02 (元日 1/1 was Sunday)
		{"2023-01-02", "振替休日"},

		// 振替休日 carry-forward: 2009-05-06
		// 5/3 Sun=憲法記念日, 5/4 Mon=みどりの日, 5/5 Tue=こどもの日, 5/6 Wed=振替休日
		{"2009-05-06", "振替休日"},

		// 国民の休日: 2032-09-21 (敬老の日=9/20 Mon, 秋分の日=9/22 Wed)
		{"2032-09-21", "国民の休日"},

		// Not a holiday
		{"2026-06-15", ""},
		{"2026-12-25", ""},
	}

	for _, tt := range tests {
		d, err := time.ParseInLocation("2006-01-02", tt.date, jst)
		if err != nil {
			t.Fatal(err)
		}
		got := jpHolidayName(d)
		if got != tt.want {
			t.Errorf("jpHolidayName(%s) = %q, want %q (weekday=%s)", tt.date, got, tt.want, d.Weekday())
		}
	}
}

func TestVernalEquinoxDay(t *testing.T) {
	tests := map[int]int{
		2024: 20, 2025: 20, 2026: 20, 2027: 21, 2030: 20,
	}
	for y, want := range tests {
		if got := vernalEquinoxDay(y); got != want {
			t.Errorf("vernalEquinoxDay(%d) = %d, want %d", y, got, want)
		}
	}
}

func TestAutumnalEquinoxDay(t *testing.T) {
	tests := map[int]int{
		2024: 22, 2025: 23, 2026: 23, 2027: 23, 2030: 23,
	}
	for y, want := range tests {
		if got := autumnalEquinoxDay(y); got != want {
			t.Errorf("autumnalEquinoxDay(%d) = %d, want %d", y, got, want)
		}
	}
}
