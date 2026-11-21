package router

import (
	"math"
	"strings"
	"unicode/utf8"
)

type Bubbles struct {
	Buffer     string
	Separators []string
	// Limit optionally caps a bubble in UTF-16 units; zero means unlimited.
	// The router leaves it unset: frontends enforce their own platform limits.
	Limit int
}

// Feed retains incomplete delimiters across event boundaries.
func (b *Bubbles) Feed(s string, flush bool) []string {
	b.Buffer += s
	var out []string
	add := func(s string) {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	limit := b.Limit
	if limit <= 0 {
		limit = math.MaxInt
	}
	for b.Buffer != "" {
		at, sepLen := -1, 0
		for _, sep := range b.Separators {
			if i := strings.Index(b.Buffer, sep); i >= 0 && (at < 0 || i < at) {
				at = i
				sepLen = len(sep)
			}
		}
		end := len(b.Buffer)
		if at >= 0 {
			end = at
		} else if !flush {
			for _, sep := range b.Separators {
				for n := 1; n < len(sep) && n <= len(b.Buffer); n++ {
					if strings.HasSuffix(b.Buffer, sep[:n]) {
						end = min(end, len(b.Buffer)-n)
					}
				}
			}
		}
		cut, units := 0, 0
		for cut < end {
			r, n := utf8.DecodeRuneInString(b.Buffer[cut:end])
			u := 1
			if r > 0xffff {
				u = 2
			}
			if units+u > limit {
				break
			}
			units += u
			cut += n
		}
		if cut < end || units == limit {
			add(b.Buffer[:cut])
			b.Buffer = b.Buffer[cut:]
			continue
		}
		if at >= 0 {
			add(b.Buffer[:at])
			b.Buffer = b.Buffer[at+sepLen:]
			continue
		}
		if flush {
			add(b.Buffer)
			b.Buffer = ""
		}
		break
	}
	return out
}
