package test

import (
	"context"
	"fmt"
	"net"
	"time"
)

// ConnectivityConfig configures a TCP/UDP reachability probe.
type ConnectivityConfig struct {
	Target  string        `json:"target"`
	Proto   string        `json:"protocol"`
	Timeout time.Duration `json:"timeout"`
	Count   int           `json:"count"`
}

// ConnectivityResult is the outcome of the reachability probe.
type ConnectivityResult struct {
	Target      string   `json:"target"`
	Protocol    string   `json:"protocol"`
	Successful  int      `json:"successful"`
	Total       int      `json:"total"`
	AvgRTTMs    float64  `json:"avgRttMs"`
	MinRTTMs    float64  `json:"minRttMs"`
	MaxRTTMs    float64  `json:"maxRttMs"`
	LossPercent float64  `json:"lossPercent"`
	Errors      []string `json:"errors,omitempty"`
}

// RunConnectivity tests reachability to a target host/port.
func RunConnectivity(ctx context.Context, cfg ConnectivityConfig) (*ConnectivityResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Target == "" {
		return nil, fmt.Errorf("target cannot be empty")
	}
	if cfg.Proto == "" {
		cfg.Proto = "tcp"
	}
	if cfg.Count <= 0 {
		cfg.Count = 3
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}

	res := &ConnectivityResult{
		Target:   cfg.Target,
		Protocol: cfg.Proto,
		Total:    cfg.Count,
	}

	var durations []time.Duration
	for i := 0; i < cfg.Count; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		start := time.Now()
		d := net.Dialer{Timeout: cfg.Timeout}
		conn, err := d.DialContext(ctx, cfg.Proto, cfg.Target)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		rtt := time.Since(start)
		_ = conn.Close()

		res.Successful++
		durations = append(durations, rtt)
	}

	if res.Total > 0 {
		res.LossPercent = float64(res.Total-res.Successful) / float64(res.Total) * 100.0
	}

	if len(durations) > 0 {
		var total time.Duration
		min := durations[0]
		max := durations[0]
		for _, d := range durations {
			total += d
			if d < min {
				min = d
			}
			if d > max {
				max = d
			}
		}
		res.AvgRTTMs = float64(total.Microseconds()) / float64(len(durations)) / 1000.0
		res.MinRTTMs = float64(min.Microseconds()) / 1000.0
		res.MaxRTTMs = float64(max.Microseconds()) / 1000.0
	}

	return res, nil
}
