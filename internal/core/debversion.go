package core

import "strings"

// CompareDebian compares two Debian package versions the way dpkg does: -1, 0 or 1.
// [epoch:]upstream[-revision]; within each part, runs of non-digits compare with '~' before
// everything (even the end) and letters before other characters, and runs of digits numerically.
func CompareDebian(a, b string) int {
	ea, ua, ra := splitDebian(a)
	eb, ub, rb := splitDebian(b)
	if c := compareNum(ea, eb); c != 0 {
		return c
	}
	if c := compareDebPart(ua, ub); c != 0 {
		return c
	}
	return compareDebPart(ra, rb)
}

func splitDebian(v string) (epoch, upstream, revision string) {
	if i := strings.IndexByte(v, ':'); i >= 0 {
		epoch, v = v[:i], v[i+1:]
	}
	if i := strings.LastIndexByte(v, '-'); i >= 0 {
		return epoch, v[:i], v[i+1:]
	}
	return epoch, v, ""
}

func debOrder(c byte, end bool) int {
	switch {
	case end:
		return 0
	case c == '~':
		return -1
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		return int(c)
	}
	return int(c) + 256
}

func compareDebPart(a, b string) int {
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	for a != "" || b != "" {
		for (a != "" && !isDigit(a[0])) || (b != "" && !isDigit(b[0])) {
			var ca, cb byte
			if a != "" {
				ca = a[0]
			}
			if b != "" {
				cb = b[0]
			}
			oa := debOrder(ca, a == "" || isDigit(ca))
			ob := debOrder(cb, b == "" || isDigit(cb))
			if oa != ob {
				if oa < ob {
					return -1
				}
				return 1
			}
			if a != "" && !isDigit(a[0]) {
				a = a[1:]
			}
			if b != "" && !isDigit(b[0]) {
				b = b[1:]
			}
		}
		i, j := 0, 0
		for i < len(a) && isDigit(a[i]) {
			i++
		}
		for j < len(b) && isDigit(b[j]) {
			j++
		}
		if c := compareNum(a[:i], b[:j]); c != 0 {
			return c
		}
		a, b = a[i:], b[j:]
	}
	return 0
}

// compareNum compares runs of digits (empty is 0) without overflowing.
func compareNum(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}
