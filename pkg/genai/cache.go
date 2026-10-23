package genai

import (
	"fmt"
	"sort"
	"strings"
)

// Metric families read from the model servers and the endpoint picker.
var (
	// vLLM V1 (and LMI on vLLM) prefix-cache counters; the text exposition
	// appends _total to counters, older builds and the gpu_ variants do not.
	vllmPrefixHits    = []string{"vllm:prefix_cache_hits_total", "vllm:prefix_cache_hits", "vllm:gpu_prefix_cache_hits_total", "vllm:gpu_prefix_cache_hits"}
	vllmPrefixQueries = []string{"vllm:prefix_cache_queries_total", "vllm:prefix_cache_queries", "vllm:gpu_prefix_cache_queries_total", "vllm:gpu_prefix_cache_queries"}
	// vLLM V0 gauge (lifetime ratio, no delta possible).
	vllmPrefixHitRateGauge = []string{"vllm:gpu_prefix_cache_hit_rate"}
	// Gateway API Inference Extension / llm-d EPP prefix indexer histogram.
	eppPrefixRatioSum   = []string{"inference_extension_prefix_indexer_hit_ratio_sum"}
	eppPrefixRatioCount = []string{"inference_extension_prefix_indexer_hit_ratio_count"}
	// KV-cache utilization gauges (0.0–1.0): vLLM V1, vLLM V0, EPP pool average.
	kvUtilization = []string{"vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc", "inference_pool_average_kv_cache_utilization"}
)

// CacheSnapshot is what one /metrics scrape says about the prompt cache.
type CacheSnapshot struct {
	Hits, Queries float64
	HasCounters   bool

	RatioSum, RatioCount float64
	HasHistogram         bool

	HitRateGauge float64
	HasGauge     bool

	KVUtilization float64
	HasKVUtil     bool
}

// SnapshotFromSamples reduces a parsed scrape to a CacheSnapshot.
func SnapshotFromSamples(samples []Sample) CacheSnapshot {
	var c CacheSnapshot
	if q, ok := sum(samples, vllmPrefixQueries...); ok {
		c.Queries = q
		c.Hits, _ = sum(samples, vllmPrefixHits...)
		c.HasCounters = true
	}
	if n, ok := sum(samples, eppPrefixRatioCount...); ok {
		c.RatioCount = n
		c.RatioSum, _ = sum(samples, eppPrefixRatioSum...)
		c.HasHistogram = true
	}
	if g, ok := mean(samples, vllmPrefixHitRateGauge...); ok {
		c.HitRateGauge = g
		c.HasGauge = true
	}
	if u, ok := mean(samples, kvUtilization...); ok {
		c.KVUtilization = clamp01(u)
		c.HasKVUtil = true
	}
	return c
}

// SnapshotFromText parses a /metrics body and reduces it.
func SnapshotFromText(text string) CacheSnapshot {
	return SnapshotFromSamples(ParseExposition(text))
}

// PrefixCacheHitRate computes the hit ratio for the interval between before
// and after. Counters and histograms are differenced; when before has no data
// the lifetime value from after is used. Returns ok=false when after carries
// no prefix-cache metric or nothing was queried during the interval.
func PrefixCacheHitRate(before, after CacheSnapshot) (rate float64, source string, ok bool) {
	switch {
	case after.HasCounters:
		hits, queries := after.Hits, after.Queries
		source = "vllm:prefix_cache_queries"
		if before.HasCounters && after.Queries >= before.Queries {
			hits -= before.Hits
			queries -= before.Queries
		} else {
			source += " (lifetime)"
		}
		if queries <= 0 {
			return 0, "", false
		}
		return clamp01(hits / queries), source, true
	case after.HasHistogram:
		s, n := after.RatioSum, after.RatioCount
		source = "inference_extension_prefix_indexer_hit_ratio"
		if before.HasHistogram && after.RatioCount >= before.RatioCount {
			s -= before.RatioSum
			n -= before.RatioCount
		} else {
			source += " (lifetime)"
		}
		if n <= 0 {
			return 0, "", false
		}
		return clamp01(s / n), source, true
	case after.HasGauge:
		return clamp01(after.HitRateGauge), "vllm:gpu_prefix_cache_hit_rate (gauge)", true
	}
	return 0, "", false
}

// Scrape is one /metrics body from one endpoint. Role is "prefill", "decode"
// or "" (a monolithic server or an EPP); Endpoint identifies the source so
// before/after pairs can be matched.
type Scrape struct {
	Role     string
	Endpoint string
	Text     string
}

// Roles accepted on --metrics-url / --metrics-before / --metrics-after.
const (
	RolePrefill = "prefill"
	RoleDecode  = "decode"
)

// ParseRoleSpec splits "[role=]value" into role and value. Only the prefixes
// prefill= and decode= name a role; anything else is the value itself, so a
// plain label selector such as app=vllm (--metrics-pod-selector) passes
// through unchanged.
func ParseRoleSpec(spec string) (role, value string, err error) {
	if i := strings.Index(spec, "="); i > 0 {
		switch r := strings.ToLower(strings.TrimSpace(spec[:i])); r {
		case RolePrefill, RoleDecode:
			return r, strings.TrimSpace(spec[i+1:]), nil
		}
	}
	return "", strings.TrimSpace(spec), nil
}

// Attach fills the prefix-cache hit rate and the per-role worker utilization
// of m from before/after scrapes. Hits and queries are summed across all
// endpoints; utilization is the mean over the endpoints of each role at the
// end of the run. It returns human-readable notes about what was used.
func Attach(m *Metrics, before, after []Scrape) []string {
	if m == nil || len(after) == 0 {
		return nil
	}
	beforeBy := map[string]CacheSnapshot{}
	for _, s := range before {
		beforeBy[s.Endpoint] = SnapshotFromText(s.Text)
	}

	var (
		agg        struct{ hits, queries, ratioSum, ratioCount float64 }
		counters   bool
		histograms bool
		gaugeSum   float64
		gaugeN     int
		utilByRole = map[string][]float64{}
		notes      []string
	)
	endpoints := make([]string, 0, len(after))
	for _, s := range after {
		endpoints = append(endpoints, s.Endpoint)
	}
	sort.Strings(endpoints)

	for _, s := range after {
		a := SnapshotFromText(s.Text)
		b := beforeBy[s.Endpoint]
		if a.HasCounters {
			counters = true
			h, q := a.Hits, a.Queries
			if b.HasCounters && a.Queries >= b.Queries {
				h -= b.Hits
				q -= b.Queries
			}
			agg.hits += h
			agg.queries += q
		}
		if a.HasHistogram {
			histograms = true
			rs, rc := a.RatioSum, a.RatioCount
			if b.HasHistogram && a.RatioCount >= b.RatioCount {
				rs -= b.RatioSum
				rc -= b.RatioCount
			}
			agg.ratioSum += rs
			agg.ratioCount += rc
		}
		if a.HasGauge {
			gaugeSum += a.HitRateGauge
			gaugeN++
		}
		if a.HasKVUtil && (s.Role == RolePrefill || s.Role == RoleDecode) {
			utilByRole[s.Role] = append(utilByRole[s.Role], a.KVUtilization)
		}
	}

	switch {
	case counters && agg.queries > 0:
		m.PrefixCacheHitRate = Float(clamp01(agg.hits / agg.queries))
		m.PrefixCacheSource = "vllm:prefix_cache_queries"
		notes = append(notes, fmt.Sprintf("prefix cache: %.0f hits / %.0f queries during the run", agg.hits, agg.queries))
	case histograms && agg.ratioCount > 0:
		m.PrefixCacheHitRate = Float(clamp01(agg.ratioSum / agg.ratioCount))
		m.PrefixCacheSource = "inference_extension_prefix_indexer_hit_ratio"
		notes = append(notes, fmt.Sprintf("prefix cache: EPP indexer ratio over %.0f requests", agg.ratioCount))
	case gaugeN > 0:
		m.PrefixCacheHitRate = Float(clamp01(gaugeSum / float64(gaugeN)))
		m.PrefixCacheSource = "vllm:gpu_prefix_cache_hit_rate (gauge)"
		notes = append(notes, "prefix cache: lifetime gauge, not an interval value")
	default:
		notes = append(notes, "prefix cache: no hit/query metric in the scrape")
	}

	for _, role := range []string{RolePrefill, RoleDecode} {
		vals := utilByRole[role]
		if len(vals) == 0 {
			continue
		}
		total := 0.0
		for _, v := range vals {
			total += v
		}
		u := Float(total / float64(len(vals)))
		if role == RolePrefill {
			m.PrefillWorkerUtilization = u
		} else {
			m.DecodeWorkerUtilization = u
		}
		notes = append(notes, fmt.Sprintf("%s pool: KV utilization %.1f%% over %d endpoint(s)", role, *u*100, len(vals)))
	}
	return notes
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}
