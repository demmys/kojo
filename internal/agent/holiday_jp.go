package agent

import (
	"math"
	"time"
)

// jst is the Asia/Tokyo timezone used for Japanese holiday calculations.
var jst = time.FixedZone("Asia/Tokyo", 9*60*60)

// jpWeekday maps time.Weekday to the Japanese weekday name.
var jpWeekday = [7]string{"日", "月", "火", "水", "木", "金", "土"}

// jpHolidayName returns the Japanese holiday name for the given date, or "" if not a holiday.
// Covers current-era national holidays (modern rules as of 2007+).
func jpHolidayName(t time.Time) string {
	t = t.In(jst)
	if name := jpBaseHoliday(t); name != "" {
		return name
	}
	return jpDerivedHoliday(t)
}

// jpBaseHoliday returns the name of a 国民の祝日 (national holiday defined by statute),
// or "" if the date is not one.
func jpBaseHoliday(t time.Time) string {
	y, m, d := t.Date()

	switch m {
	case time.January:
		if d == 1 {
			return "元日"
		}
		if t.Weekday() == time.Monday && d >= 8 && d <= 14 {
			return "成人の日"
		}

	case time.February:
		if d == 11 {
			return "建国記念の日"
		}
		if d == 23 && y >= 2020 {
			return "天皇誕生日"
		}

	case time.March:
		if d == vernalEquinoxDay(y) {
			return "春分の日"
		}

	case time.April:
		if d == 29 {
			return "昭和の日"
		}

	case time.May:
		switch d {
		case 3:
			return "憲法記念日"
		case 4:
			return "みどりの日"
		case 5:
			return "こどもの日"
		}

	case time.July:
		if t.Weekday() == time.Monday && d >= 15 && d <= 21 {
			return "海の日"
		}

	case time.August:
		if d == 11 && y >= 2016 {
			return "山の日"
		}

	case time.September:
		if d == autumnalEquinoxDay(y) {
			return "秋分の日"
		}
		if t.Weekday() == time.Monday && d >= 15 && d <= 21 {
			return "敬老の日"
		}

	case time.October:
		if t.Weekday() == time.Monday && d >= 8 && d <= 14 {
			if y >= 2020 {
				return "スポーツの日"
			}
			return "体育の日"
		}

	case time.November:
		if d == 3 {
			return "文化の日"
		}
		if d == 23 {
			return "勤労感謝の日"
		}

	case time.December:
		if d == 23 && y <= 2018 {
			return "天皇誕生日"
		}
	}

	return ""
}

// jpDerivedHoliday returns "振替休日" or "国民の休日" if applicable, or "".
func jpDerivedHoliday(t time.Time) string {
	// 振替休日: when a national holiday falls on Sunday, the next non-holiday weekday
	// becomes a substitute holiday. Post-2007 this carries forward through consecutive holidays.
	// Walk backward from t: if every day back to the nearest Sunday is a base holiday,
	// and that Sunday is also a base holiday, then t is a 振替休日.
	if t.Weekday() != time.Sunday {
		prev := t.AddDate(0, 0, -1)
		for prev.Weekday() != time.Saturday {
			if jpBaseHoliday(prev) == "" {
				break
			}
			if prev.Weekday() == time.Sunday {
				return "振替休日"
			}
			prev = prev.AddDate(0, 0, -1)
		}
	}

	// 国民の休日: a non-Sunday sandwiched between two base holidays.
	if t.Weekday() != time.Sunday {
		prev := t.AddDate(0, 0, -1)
		next := t.AddDate(0, 0, 1)
		if jpBaseHoliday(prev) != "" && jpBaseHoliday(next) != "" {
			return "国民の休日"
		}
	}

	return ""
}

// vernalEquinoxDay returns the day of 春分の日 for a given year.
// Standard formula from the National Astronomical Observatory of Japan.
func vernalEquinoxDay(y int) int {
	switch {
	case y <= 1979:
		return int(math.Floor(20.8357 + 0.242194*float64(y-1980) - math.Floor(float64(y-1983)/4)))
	case y <= 2099:
		return int(math.Floor(20.8431 + 0.242194*float64(y-1980) - math.Floor(float64(y-1980)/4)))
	default:
		return int(math.Floor(21.8510 + 0.242194*float64(y-1980) - math.Floor(float64(y-1980)/4)))
	}
}

// autumnalEquinoxDay returns the day of 秋分の日 for a given year.
func autumnalEquinoxDay(y int) int {
	switch {
	case y <= 1979:
		return int(math.Floor(23.2588 + 0.242194*float64(y-1980) - math.Floor(float64(y-1983)/4)))
	case y <= 2099:
		return int(math.Floor(23.2488 + 0.242194*float64(y-1980) - math.Floor(float64(y-1980)/4)))
	default:
		return int(math.Floor(24.2488 + 0.242194*float64(y-1980) - math.Floor(float64(y-1980)/4)))
	}
}
