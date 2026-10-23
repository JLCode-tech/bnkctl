package genai

import (
	"math"
	"strconv"
	"strings"
)

// Sample is one line of a Prometheus text exposition.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// ParseExposition parses Prometheus text format (the /metrics body of vLLM,
// LMI or an EPP). Comment and blank lines are skipped, unparsable lines are
// ignored, and NaN values are dropped.
func ParseExposition(text string) []Sample {
	var out []Sample
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, ok := parseSampleLine(line)
		if !ok || math.IsNaN(s.Value) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func parseSampleLine(line string) (Sample, bool) {
	s := Sample{Labels: map[string]string{}}
	rest := line
	if i := strings.IndexAny(line, "{ \t"); i >= 0 {
		s.Name = line[:i]
		rest = line[i:]
	} else {
		return Sample{}, false
	}
	if strings.HasPrefix(rest, "{") {
		end := labelBlockEnd(rest)
		if end < 0 {
			return Sample{}, false
		}
		parseLabels(rest[1:end], s.Labels)
		rest = rest[end+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return Sample{}, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return Sample{}, false
	}
	s.Value = v
	return s, true
}

// labelBlockEnd returns the index of the closing brace of a label block that
// starts at s[0] == '{', honouring quoted values with escapes.
func labelBlockEnd(s string) int {
	inQuote := false
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if inQuote {
				i++
			}
		case '"':
			inQuote = !inQuote
		case '}':
			if !inQuote {
				return i
			}
		}
	}
	return -1
}

func parseLabels(body string, into map[string]string) {
	i := 0
	for i < len(body) {
		eq := strings.IndexByte(body[i:], '=')
		if eq < 0 {
			return
		}
		key := strings.TrimSpace(body[i : i+eq])
		i += eq + 1
		if i >= len(body) || body[i] != '"' {
			return
		}
		i++
		var val strings.Builder
		for i < len(body) && body[i] != '"' {
			if body[i] == '\\' && i+1 < len(body) {
				i++
				switch body[i] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(body[i])
				}
			} else {
				val.WriteByte(body[i])
			}
			i++
		}
		i++ // closing quote
		into[key] = val.String()
		for i < len(body) && (body[i] == ',' || body[i] == ' ') {
			i++
		}
	}
}

// sum adds the values of every sample whose name is one of names.
func sum(samples []Sample, names ...string) (float64, bool) {
	total, found := 0.0, false
	for _, s := range samples {
		for _, n := range names {
			if s.Name == n {
				total += s.Value
				found = true
			}
		}
	}
	return total, found
}

// mean averages the values of every sample whose name is one of names.
func mean(samples []Sample, names ...string) (float64, bool) {
	total, n := 0.0, 0
	for _, s := range samples {
		for _, name := range names {
			if s.Name == name {
				total += s.Value
				n++
			}
		}
	}
	if n == 0 {
		return 0, false
	}
	return total / float64(n), true
}
