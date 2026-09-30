package main

import "time"

// timeNow is a variable so time-dependent logic can be tested deterministically.
var timeNow = time.Now

// timeUnix is a small wrapper so day boundaries can be computed from a unix
// timestamp without importing time in every file.
func timeUnix(ts int64) time.Time { return time.Unix(ts, 0) }

// timeAgo renders a unix timestamp as a friendly relative description
// ("3 days ago", "2 weeks ago"), which reads better in results than a raw
// date for anything recent.
func timeAgo(ts int64) string {
	d := timeNow().Sub(timeUnix(ts))
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		n := int(d.Minutes())
		if n == 1 {
			return "1 minute ago"
		}
		return plural(n, "minute") + " ago"
	case d < 24*time.Hour:
		n := int(d.Hours())
		if n == 1 {
			return "1 hour ago"
		}
		return plural(n, "hour") + " ago"
	case d < 30*24*time.Hour:
		n := int(d.Hours() / 24)
		if n == 1 {
			return "yesterday"
		}
		return plural(n, "day") + " ago"
	case d < 365*24*time.Hour:
		n := int(d.Hours() / 24 / 30)
		if n < 1 {
			n = 1
		}
		return plural(n, "month") + " ago"
	default:
		n := int(d.Hours() / 24 / 365)
		return plural(n, "year") + " ago"
	}
}

// plural is a tiny helper so "1 day" / "2 days" read correctly.
func plural(n int, word string) string {
	s := word
	if n != 1 {
		s += "s"
	}
	return itoa(n) + " " + s
}

// itoa avoids importing strconv in several places.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
