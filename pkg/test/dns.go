package test

import (
	"context"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// DNSProbeConfig configures a DNS probe run.
type DNSProbeConfig struct {
	Target     string        `json:"target"`
	Type       string        `json:"type"`
	Server     string        `json:"server"`
	Iterations int           `json:"iterations"`
	Timeout    time.Duration `json:"timeout"`
	EDNSSubnet string        `json:"ednsSubnet,omitempty"`
}

// DNSProbeResult is the result of a DNS probe run.
type DNSProbeResult struct {
	Target        string          `json:"target"`
	Server        string          `json:"server"`
	QueryType     string          `json:"queryType"`
	Iterations    int             `json:"iterations"`
	LatencyMs     RTTDistribution `json:"latencyMs"`
	Answers       []DNSAnswer     `json:"answers"`
	Rcode         string          `json:"rcode"`
	Authoritative bool            `json:"authoritative"`
	Truncated     bool            `json:"truncated"`
	Error         string          `json:"error,omitempty"`
}

// RTTDistribution captures latency percentiles in milliseconds.
type RTTDistribution struct {
	Min float64 `json:"min"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// DNSAnswer is an individual DNS resource record answer.
type DNSAnswer struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	TTL   uint32 `json:"ttl"`
	RData string `json:"rdata"`
}

// RunDNS executes the DNS probe against the specified server and returns metrics.
func RunDNS(ctx context.Context, cfg DNSProbeConfig) (*DNSProbeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Target == "" {
		return nil, fmt.Errorf("target domain cannot be empty")
	}
	if cfg.Server == "" {
		cfg.Server = "8.8.8.8:53"
	}
	if !strings.Contains(cfg.Server, ":") {
		cfg.Server = cfg.Server + ":53"
	}
	if cfg.Iterations <= 0 {
		cfg.Iterations = 1
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}

	qtype := dns.TypeA
	switch strings.ToUpper(cfg.Type) {
	case "AAAA":
		qtype = dns.TypeAAAA
	case "TXT":
		qtype = dns.TypeTXT
	case "CNAME":
		qtype = dns.TypeCNAME
	case "MX":
		qtype = dns.TypeMX
	case "NS":
		qtype = dns.TypeNS
	default:
		cfg.Type = "A"
	}

	fqdn := dns.Fqdn(cfg.Target)
	m := new(dns.Msg)
	m.SetQuestion(fqdn, qtype)
	m.RecursionDesired = true

	if cfg.EDNSSubnet != "" {
		o := new(dns.OPT)
		o.Hdr.Name = "."
		o.Hdr.Rrtype = dns.TypeOPT
		e := new(dns.EDNS0_SUBNET)
		e.Code = dns.EDNS0SUBNET
		ip := net.ParseIP(cfg.EDNSSubnet)
		if ip != nil {
			if ip.To4() != nil {
				e.Family = 1
				e.SourceNetmask = 24
				e.Address = ip.To4()
			} else {
				e.Family = 2
				e.SourceNetmask = 64
				e.Address = ip.To16()
			}
			o.Option = append(o.Option, e)
			m.Extra = append(m.Extra, o)
		}
	}

	c := new(dns.Client)
	c.Timeout = cfg.Timeout

	var rtts []float64
	var lastIn *dns.Msg
	var lastErr error

	for i := 0; i < cfg.Iterations; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		in, rtt, err := c.Exchange(m, cfg.Server)
		if err != nil {
			lastErr = err
			continue
		}
		lastIn = in
		rtts = append(rtts, float64(rtt.Microseconds())/1000.0)
	}

	res := &DNSProbeResult{
		Target:     cfg.Target,
		Server:     cfg.Server,
		QueryType:  cfg.Type,
		Iterations: cfg.Iterations,
	}

	if len(rtts) == 0 {
		if lastErr != nil {
			res.Error = lastErr.Error()
			return res, lastErr
		}
		res.Error = "no responses received"
		return res, fmt.Errorf("no responses received from %s", cfg.Server)
	}

	res.LatencyMs = calcRTT(rtts)
	if lastIn != nil {
		res.Rcode = dns.RcodeToString[lastIn.Rcode]
		res.Authoritative = lastIn.Authoritative
		res.Truncated = lastIn.Truncated
		for _, ans := range lastIn.Answer {
			hdr := ans.Header()
			rdata := strings.TrimSpace(strings.TrimPrefix(ans.String(), hdr.String()))
			res.Answers = append(res.Answers, DNSAnswer{
				Name:  hdr.Name,
				Type:  dns.TypeToString[hdr.Rrtype],
				TTL:   hdr.Ttl,
				RData: rdata,
			})
		}
	}

	return res, nil
}

func calcRTT(rtts []float64) RTTDistribution {
	if len(rtts) == 0 {
		return RTTDistribution{}
	}
	sort.Float64s(rtts)
	n := len(rtts)
	return RTTDistribution{
		Min: rtts[0],
		P50: percentile(rtts, 0.50),
		P95: percentile(rtts, 0.95),
		P99: percentile(rtts, 0.99),
		Max: rtts[n-1],
	}
}

func percentile(sorted []float64, pct float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	idx := pct * float64(n-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper {
		return sorted[lower]
	}
	weight := idx - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}
