package main

import (
	"errors"
	"strings"
	"time"
)

func parseTime(inp string) (time.Time, error) {
	var t time.Time
	var err error
	var op string
	first := true
	for inp != "" {
		idx := strings.IndexAny(inp, ", ")
		if idx < 0 {
			idx = len(inp)
		}
		tok := inp[:idx]
		inp = inp[idx:]
		if len(inp) > 0 {
			// skip separator
			inp = inp[1:]
		}

		if first {
			switch tok {
			case "now":
				t = time.Now()
			default:
				t, err = time.Parse(time.RFC3339, tok)
				if err != nil {
					return time.Time{}, err
				}
			}
		} else {
			var d time.Duration
			d, err = time.ParseDuration(tok)
			if err != nil {
				return time.Time{}, err
			}
			switch op {
			case "+":
				t = t.Add(d)
			case "-":
				t = t.Add(-d)
			default:
				panic("bug")
			}
		}

		if inp != "" {
			op = inp[0:1]
			inp = inp[1:]
		}
		first = false
	}
	if first {
		return time.Time{}, errors.New("no time specified")
	} else {
		return t, nil
	}
}
