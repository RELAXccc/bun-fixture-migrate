package fixturemigrate

import (
	"fmt"
	"math/big"
	"strings"
)

// intervalText is what PostgreSQL's interval input makes of text, written as
// PostgreSQL writes an interval under IntervalStyle postgres, which is how the
// tool reads the database: "1 day 2 hours" is "1 day 02:00:00", "24 hours" is
// "24:00:00", "86400 seconds" is "24:00:00" too, "PT24H" the same. ok is false
// for text that is not an interval to PostgreSQL, and for text whose value
// this reading does not settle on its own, which is then compared as written:
// a fraction of a week, a month or a year (PostgreSQL works it out in floating
// point, which a whole number of days may come out a microsecond short of),
// a fraction of a microsecond, the SQL standard's day-time spelling with a
// sign, a value out of the range of everyday intervals.
//
// It follows PostgreSQL's DecodeInterval and DecodeISO8601Interval
// (src/backend/utils/adt/datetime.c): the fields read right to left, a unit
// naming the number before it, a number without one a second, or a day
// before a time; each unit at most once; "ago" last, negating the whole; a
// sign per field. Without the database, it is what tells two spellings of one
// interval from two intervals.
func intervalText(s string) (string, bool) {
	iv, ok := parseInterval(s)
	if !ok {
		var iso bool
		if iv, iso = parseISOInterval(s); !iso {
			return "", false
		}
	}
	return iv.String(), true
}

// interval is PostgreSQL's: months, days and microseconds, kept apart.
type interval struct {
	months, days, usec int64
}

const (
	usecPerSecond = int64(1_000_000)
	usecPerMinute = 60 * usecPerSecond
	usecPerHour   = 60 * usecPerMinute
	usecPerDay    = 24 * usecPerHour
	// intervalLimit bounds every number read, far below where PostgreSQL's
	// own arithmetic, or this one's, overflows.
	intervalLimit = int64(1_000_000_000)
)

// String writes an interval as EncodeInterval does under IntervalStyle
// postgres: years, mons and days each with its own sign, then the time as
// [-]HH:MM:SS[.ffffff], which a field after a negative one marks with a +.
func (iv interval) String() string {
	var b strings.Builder
	zero, before := true, false
	part := func(v int64, unit string) {
		if v == 0 {
			return
		}
		if !zero {
			b.WriteByte(' ')
		}
		if before && v > 0 {
			b.WriteByte('+')
		}
		fmt.Fprintf(&b, "%d %s", v, unit)
		if v != 1 {
			b.WriteByte('s')
		}
		before, zero = v < 0, false
	}
	part(iv.months/12, "year")
	part(iv.months%12, "mon")
	part(iv.days, "day")
	t := iv.usec
	hour := t / usecPerHour
	t -= hour * usecPerHour
	minute := t / usecPerMinute
	t -= minute * usecPerMinute
	sec := t / usecPerSecond
	frac := t - sec*usecPerSecond
	if zero || hour != 0 || minute != 0 || sec != 0 || frac != 0 {
		if !zero {
			b.WriteByte(' ')
		}
		switch {
		case hour < 0 || minute < 0 || sec < 0 || frac < 0:
			b.WriteByte('-')
		case before:
			b.WriteByte('+')
		}
		fmt.Fprintf(&b, "%02d:%02d:%02d", abs64(hour), abs64(minute), abs64(sec))
		if frac != 0 {
			b.WriteString(strings.TrimRight(fmt.Sprintf(".%06d", abs64(frac)), "0"))
		}
	}
	return b.String()
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// The fields of interval text as ParseDateTime splits them.
const (
	tokNumber    = iota // 12, 1.5, .5
	tokYearMonth        // 1-2, the SQL standard's years and months
	tokTime             // 12:30, 1:02:03.5
	tokSigned           // -1, +1.5, -1-2, -12:30
	tokWord             // day, hours, ago
)

type intervalToken struct {
	kind int
	text string
}

// tokenizeInterval splits text into fields as ParseDateTime does, for the
// fields an interval is made of. ok is false for anything else: a date, a
// word joined to a number by punctuation, a character it does not know.
func tokenizeInterval(s string) ([]intervalToken, bool) {
	var out []intervalToken
	digit := func(c byte) bool { return c >= '0' && c <= '9' }
	alpha := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case digit(c):
			start := i
			for i < len(s) && digit(s[i]) {
				i++
			}
			switch {
			case i < len(s) && s[i] == ':':
				for i < len(s) && (digit(s[i]) || s[i] == ':' || s[i] == '.') {
					i++
				}
				out = append(out, intervalToken{tokTime, s[start:i]})
			case i < len(s) && (s[i] == '-' || s[i] == '.'):
				delim := s[i]
				i++
				if i >= len(s) || !digit(s[i]) {
					return nil, false
				}
				for i < len(s) && digit(s[i]) {
					i++
				}
				if i < len(s) && s[i] == delim {
					return nil, false // a date
				}
				kind := tokNumber
				if delim == '-' {
					kind = tokYearMonth
				}
				out = append(out, intervalToken{kind, s[start:i]})
			case i < len(s) && s[i] == '/':
				return nil, false
			default:
				out = append(out, intervalToken{tokNumber, s[start:i]})
			}
		case c == '.':
			start := i
			i++
			for i < len(s) && digit(s[i]) {
				i++
			}
			out = append(out, intervalToken{tokNumber, s[start:i]})
		case alpha(c):
			start := i
			for i < len(s) && alpha(s[i]) {
				i++
			}
			if i < len(s) && (s[i] == '-' || s[i] == '/' || s[i] == '.') {
				return nil, false
			}
			out = append(out, intervalToken{tokWord, strings.ToLower(s[start:i])})
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '+' || c == '-':
			start := i
			i++
			for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
				i++
			}
			if i >= len(s) || !digit(s[i]) {
				return nil, false
			}
			begin := i
			for i < len(s) && (digit(s[i]) || s[i] == ':' || s[i] == '.' || s[i] == '-') {
				i++
			}
			out = append(out, intervalToken{tokSigned, s[start:start+1] + s[begin:i]})
		case c == '@' || c == ',':
			// Punctuation PostgreSQL passes over: "@ 1 day", "1 day, 2 hours".
			i++
		default:
			return nil, false
		}
	}
	return out, true
}

// The units of DecodeInterval, by the first ten letters PostgreSQL compares
// a word on (TOKMAXLEN). "ago" and unknown words are not among them.
var intervalUnits = map[string]int{
	"c": unitCentury, "cent": unitCentury, "centuries": unitCentury, "century": unitCentury,
	"d": unitDay, "day": unitDay, "days": unitDay,
	"dec": unitDecade, "decade": unitDecade, "decades": unitDecade, "decs": unitDecade,
	"h": unitHour, "hour": unitHour, "hours": unitHour, "hr": unitHour, "hrs": unitHour,
	"m": unitMinute, "min": unitMinute, "mins": unitMinute, "minute": unitMinute, "minutes": unitMinute,
	"microsecon": unitMicrosecond, "us": unitMicrosecond, "usec": unitMicrosecond, "useconds": unitMicrosecond,
	"usecs": unitMicrosecond,
	"mil":   unitMillennium, "millennia": unitMillennium, "millennium": unitMillennium, "mils": unitMillennium,
	"millisecon": unitMillisecond, "ms": unitMillisecond, "msec": unitMillisecond, "mseconds": unitMillisecond,
	"msecs": unitMillisecond,
	"mon":   unitMonth, "mons": unitMonth, "month": unitMonth, "months": unitMonth,
	"s": unitSecond, "sec": unitSecond, "second": unitSecond, "seconds": unitSecond, "secs": unitSecond,
	"w": unitWeek, "week": unitWeek, "weeks": unitWeek,
	"y": unitYear, "year": unitYear, "years": unitYear, "yr": unitYear, "yrs": unitYear,
}

const (
	unitNone = iota
	unitMicrosecond
	unitMillisecond
	unitSecond
	unitMinute
	unitHour
	unitDay
	unitWeek
	unitMonth
	unitYear
	unitDecade
	unitCentury
	unitMillennium
	// unitAfterAgo is what a number right before "ago" is read as: nothing,
	// which PostgreSQL refuses.
	unitAfterAgo
)

// The fields DecodeInterval lets a string hold once each.
const (
	maskYear = 1 << iota
	maskMonth
	maskDay
	maskHour
	maskMinute
	maskSecond
	maskMillisecond
	maskMicrosecond
	maskWeek
	maskDecade
	maskCentury
	maskMillennium

	maskAllSeconds = maskSecond | maskMillisecond | maskMicrosecond
	maskTime       = maskHour | maskMinute | maskAllSeconds
)

// parseInterval reads text as DecodeInterval does under IntervalStyle
// postgres.
func parseInterval(s string) (interval, bool) {
	tokens, ok := tokenizeInterval(s)
	if !ok || len(tokens) == 0 {
		return interval{}, false
	}
	var iv interval
	fmask, unit, ago, pending := 0, unitNone, false, false
	for i := len(tokens) - 1; i >= 0; i-- {
		tok := tokens[i]
		tmask := 0
		switch tok.kind {
		case tokTime:
			usec, ok := intervalTime(tok.text)
			if !ok {
				return interval{}, false
			}
			iv.usec += usec
			tmask, unit, pending = maskTime, unitDay, false
		case tokSigned:
			if strings.Contains(tok.text, ":") {
				usec, ok := intervalTime(tok.text[1:])
				if !ok {
					return interval{}, false
				}
				if tok.text[0] == '-' {
					usec = -usec
				}
				iv.usec += usec
				tmask, unit, pending = maskTime, unitDay, false
				break
			}
			fallthrough
		case tokNumber, tokYearMonth:
			if unit == unitNone {
				unit = unitSecond
			}
			n, ok := intervalNumber(tok.text)
			if !ok {
				return interval{}, false
			}
			if n.yearMonth {
				// The SQL standard's years and months: a month count from
				// here on, as PostgreSQL reads it.
				unit = unitMonth
			}
			var tm int
			switch unit {
			case unitMicrosecond:
				tm, ok = maskMicrosecond, n.add(&iv.usec, 1)
			case unitMillisecond:
				tm, ok = maskMillisecond, n.add(&iv.usec, 1000)
			case unitSecond:
				tm, ok = maskSecond, n.add(&iv.usec, usecPerSecond)
				if n.frac != nil {
					tm = maskAllSeconds
				}
			case unitMinute:
				tm, ok = maskMinute, n.add(&iv.usec, usecPerMinute)
			case unitHour:
				tm, ok = maskHour, n.add(&iv.usec, usecPerHour)
				unit = unitDay
			case unitDay:
				iv.days += n.whole
				tm, ok = maskDay, n.addFrac(&iv.usec, usecPerDay)
			case unitWeek:
				tm, ok = maskWeek, n.wholeUnits(&iv.days, 7)
			case unitMonth:
				tm, ok = maskMonth, n.wholeUnits(&iv.months, 1)
			case unitYear:
				tm, ok = maskYear, n.wholeUnits(&iv.months, 12)
			case unitDecade:
				tm, ok = maskDecade, n.wholeUnits(&iv.months, 120)
			case unitCentury:
				tm, ok = maskCentury, n.wholeUnits(&iv.months, 1200)
			case unitMillennium:
				tm, ok = maskMillennium, n.wholeUnits(&iv.months, 12000)
			default:
				ok = false
			}
			if !ok {
				return interval{}, false
			}
			tmask, pending = tm, false
		case tokWord:
			if pending {
				return interval{}, false
			}
			word := tok.text
			if len(word) > 10 {
				word = word[:10]
			}
			if word == "ago" {
				if i != len(tokens)-1 {
					return interval{}, false
				}
				ago, unit = true, unitAfterAgo
				continue
			}
			u, known := intervalUnits[word]
			if !known {
				return interval{}, false
			}
			unit, pending = u, true
			continue
		}
		if tmask&fmask != 0 {
			return interval{}, false
		}
		fmask |= tmask
	}
	if fmask == 0 || pending {
		return interval{}, false
	}
	if ago {
		iv.months, iv.days, iv.usec = -iv.months, -iv.days, -iv.usec
	}
	return iv, iv.inRange()
}

// intervalNum is a number field of interval text: its whole part and its
// fraction, both with the field's sign, or a count of months for the SQL
// standard's years-months.
type intervalNum struct {
	whole     int64
	frac      *big.Rat
	yearMonth bool
}

// intervalNumber reads a number field: [+-]digits[.digits], [+-].digits, or
// [+-]years-months.
func intervalNumber(text string) (intervalNum, bool) {
	sign := int64(1)
	if text[0] == '+' || text[0] == '-' {
		if text[0] == '-' {
			sign = -1
		}
		text = text[1:]
	}
	if years, months, ok := strings.Cut(text, "-"); ok {
		y, ok1 := smallInt(years)
		m, ok2 := smallInt(months)
		if !ok1 || !ok2 || m >= 12 {
			return intervalNum{}, false
		}
		return intervalNum{whole: sign * (y*12 + m), yearMonth: true}, true
	}
	whole, frac, hasFrac := strings.Cut(text, ".")
	var n intervalNum
	if whole != "" {
		w, ok := smallInt(whole)
		if !ok {
			return intervalNum{}, false
		}
		n.whole = sign * w
	}
	if hasFrac {
		if frac == "" || strings.Trim(frac, "0123456789") != "" {
			return intervalNum{}, false
		}
		f, ok := new(big.Rat).SetString("0." + frac)
		if !ok {
			return intervalNum{}, false
		}
		if f.Sign() != 0 {
			if sign < 0 {
				f.Neg(f)
			}
			n.frac = f
		}
	} else if whole == "" {
		return intervalNum{}, false
	}
	return n, true
}

// add adds the number, in units of scale microseconds, to usec; false when
// its fraction comes to a fraction of a microsecond, which PostgreSQL rounds.
func (n intervalNum) add(usec *int64, scale int64) bool {
	*usec += n.whole * scale
	return n.addFrac(usec, scale)
}

// addFrac adds the fraction alone, as add does.
func (n intervalNum) addFrac(usec *int64, scale int64) bool {
	if n.frac == nil {
		return true
	}
	v := new(big.Rat).Mul(n.frac, new(big.Rat).SetInt64(scale))
	if !v.IsInt() {
		return false
	}
	*usec += v.Num().Int64()
	return true
}

// wholeUnits adds a number of whole units of scale to a count of days or
// months; false for a fraction, which PostgreSQL spreads over smaller units
// in floating point.
func (n intervalNum) wholeUnits(count *int64, scale int64) bool {
	*count += n.whole * scale
	return n.frac == nil
}

// intervalTime reads a time field as DecodeTime does for an interval:
// hours:minutes, hours:minutes:seconds[.fraction], and minutes:seconds.fraction.
func intervalTime(s string) (int64, bool) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var h, m, sec int64
	var frac string
	last := parts[len(parts)-1]
	last, frac, hasFrac := strings.Cut(last, ".")
	parts[len(parts)-1] = last
	nums := make([]int64, len(parts))
	for i, p := range parts {
		n, ok := smallInt(p)
		if !ok {
			return 0, false
		}
		nums[i] = n
	}
	switch {
	case len(nums) == 3:
		h, m, sec = nums[0], nums[1], nums[2]
	case hasFrac:
		// "always assume mm:ss.sss is MINUTE TO SECOND"
		m, sec = nums[0], nums[1]
	default:
		h, m = nums[0], nums[1]
	}
	usec := int64(0)
	if hasFrac {
		if frac == "" || len(strings.TrimRight(frac, "0")) > 6 {
			return 0, false
		}
		for _, c := range frac {
			if c < '0' || c > '9' {
				return 0, false
			}
		}
		padded := (frac + "000000")[:6]
		n, ok := smallInt(padded)
		if !ok {
			return 0, false
		}
		usec = n
	}
	if m > 59 || sec > 60 {
		return 0, false
	}
	return ((h*60+m)*60+sec)*usecPerSecond + usec, true
}

// parseISOInterval reads ISO 8601's format with designators, P1Y2M3DT4H5M6S,
// as DecodeISO8601Interval does; a fraction only of a day, an hour, a minute
// or a second, and none of the alternative format, P0001-02-03T04:05:06.
func parseISOInterval(s string) (interval, bool) {
	if len(s) < 2 || s[0] != 'P' {
		return interval{}, false
	}
	var iv interval
	date, fields := true, 0
	for i := 1; i < len(s); {
		if s[i] == 'T' {
			date = false
			i++
			continue
		}
		start := i
		if s[i] == '-' {
			i++
		}
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
			i++
		}
		if i == start || i >= len(s) {
			return interval{}, false
		}
		number, designator := s[start:i], s[i]
		i++
		if strings.Count(number, ".") > 1 || strings.Contains(number[1:], "-") {
			return interval{}, false
		}
		n, ok := intervalNumber(number)
		if !ok {
			return interval{}, false
		}
		switch {
		case date && designator == 'Y':
			ok = n.wholeUnits(&iv.months, 12)
		case date && designator == 'M':
			ok = n.wholeUnits(&iv.months, 1)
		case date && designator == 'W':
			ok = n.wholeUnits(&iv.days, 7)
		case date && designator == 'D':
			iv.days += n.whole
			ok = n.addFrac(&iv.usec, usecPerDay)
		case !date && designator == 'H':
			ok = n.add(&iv.usec, usecPerHour)
		case !date && designator == 'M':
			ok = n.add(&iv.usec, usecPerMinute)
		case !date && designator == 'S':
			ok = n.add(&iv.usec, usecPerSecond)
		default:
			ok = false
		}
		if !ok {
			return interval{}, false
		}
		fields++
	}
	return iv, fields > 0 && iv.inRange()
}

// smallInt reads a number of decimal digits no greater than intervalLimit.
func smallInt(s string) (int64, bool) {
	if s == "" || len(s) > 10 {
		return 0, false
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, n <= intervalLimit
}

// inRange keeps to intervals far from any limit: a few million years, and
// hours within what every int64 step above holds exactly.
func (iv interval) inRange() bool {
	return abs64(iv.months) <= intervalLimit && abs64(iv.days) <= intervalLimit &&
		abs64(iv.usec) <= intervalLimit*usecPerHour
}
