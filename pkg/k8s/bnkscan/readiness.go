package bnkscan

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Tally counts objects of one kind and how many of them are ready.
type Tally struct {
	Total int `json:"total"`
	Ready int `json:"ready"`
}

// String renders ready/total.
func (t Tally) String() string { return fmt.Sprintf("%d/%d", t.Ready, t.Total) }

// Readiness is the cluster readiness verdict every awsbnkctl surface shares:
// awsbnkctl up (phase 25), bnk upgrade, status, doctor and forge scan all
// derive it from the same Index so they never disagree.
type Readiness struct {
	Generation Generation `json:"generation"`
	Ready      bool       `json:"ready"`
	// Problems lists the failed checks, one line each; empty when Ready.
	Problems []string `json:"problems,omitempty"`
	// Infras counts Infra CRs and those with Programmed=True.
	Infras Tally `json:"infras"`
	// Gateways counts BNK Gateways and those Accepted=True and Programmed=True.
	Gateways Tally `json:"gateways"`
	// Controller is the f5-cne-controller rollout.
	Controller Controller `json:"controller"`
	// TMM is the f5-tmm DaemonSet rollout (Found=false before FLO renders it).
	TMM TMM `json:"tmm"`
	// LegacyCRDs is true when gateway.k8s.f5net.com is still served.
	LegacyCRDs bool `json:"legacyCRDs"`
	// LegacyCRs counts the 2.3 CRs still present.
	LegacyCRs int `json:"legacyCRs"`
}

// MigrateHint is the advice printed when a 2.4 cluster still carries 2.3 API
// objects.
const MigrateHint = "run `awsbnkctl bnk migrate-2.4`"

// Readiness computes the strict verdict: a 2.3 cluster is not ready.
func (idx *Index) Readiness() Readiness { return idx.readiness(false) }

func (idx *Index) readiness(acceptLegacy bool) Readiness {
	r := Readiness{Generation: idx.Generation, Controller: idx.Controller, TMM: idx.TMM, LegacyCRs: idx.Legacy.count()}
	for _, g := range idx.Groups {
		if g == LegacyPolicyGroup {
			r.LegacyCRDs = true
		}
	}
	for _, o := range idx.Infras {
		r.Infras.Total++
		if o.Ready {
			r.Infras.Ready++
		}
	}
	for _, o := range idx.Gateways {
		r.Gateways.Total++
		if o.Ready {
			r.Gateways.Ready++
		}
	}
	r.Problems = idx.problems(acceptLegacy)
	r.Ready = len(r.Problems) == 0
	return r
}

// Summary is the one-line operator view.
func (r Readiness) Summary() string {
	var parts []string
	if r.Generation == Gen24 || r.Generation == GenMixed {
		parts = append(parts, fmt.Sprintf("Infra %s Programmed", r.Infras))
	}
	parts = append(parts, fmt.Sprintf("Gateway %s Accepted+Programmed", r.Gateways))
	switch {
	case !r.Controller.Found:
		parts = append(parts, fmt.Sprintf("controller %s/%s not found", r.Controller.Namespace, r.Controller.Name))
	default:
		parts = append(parts, fmt.Sprintf("controller %d/%d available", r.Controller.Available, r.Controller.Desired))
	}
	if r.TMM.Found {
		parts = append(parts, fmt.Sprintf("TMM %d/%d ready", r.TMM.Ready, r.TMM.Desired))
	}
	verdict := "ready"
	if !r.Ready {
		verdict = fmt.Sprintf("not ready (%d problem(s))", len(r.Problems))
	}
	return fmt.Sprintf("BNK %s: %s; %s", r.Generation, strings.Join(parts, ", "), verdict)
}

// MigrationAdvice is non-empty when 2.3 CRDs or CRs remain on a cluster that
// serves the 2.4 API.
func (r Readiness) MigrationAdvice() string {
	if r.Generation != Gen24 && r.Generation != GenMixed {
		return ""
	}
	switch {
	case r.LegacyCRDs && r.LegacyCRs > 0:
		return fmt.Sprintf("legacy 2.3 CRDs (%s) still served and %d legacy CR(s) present; %s", LegacyPolicyGroup, r.LegacyCRs, MigrateHint)
	case r.LegacyCRDs:
		return fmt.Sprintf("legacy 2.3 CRDs (%s) still served; %s", LegacyPolicyGroup, MigrateHint)
	case r.LegacyCRs > 0:
		return fmt.Sprintf("%d legacy 2.3 CR(s) present; %s", r.LegacyCRs, MigrateHint)
	}
	return ""
}

// CheckReadiness scans the cluster once and returns the verdict with the
// Index it was derived from. With opts.AcceptLegacy a 2.3 cluster counts as
// ready when its controller is up: the lifecycle gates (awsbnkctl up) run on
// either generation.
func CheckReadiness(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, opts Options) (Readiness, *Index, error) {
	idx, err := Scan(ctx, dyn, cs, opts)
	if err != nil {
		return Readiness{}, nil, err
	}
	return idx.readiness(opts.AcceptLegacy), idx, nil
}

// WaitReady polls CheckReadiness until the cluster is ready, the timeout
// elapses or ctx ends. Each attempt logs the summary to log (nil discards).
// On timeout the error carries the last problems.
func WaitReady(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, opts Options, timeout, poll time.Duration, log io.Writer) (Readiness, *Index, error) {
	if log == nil {
		log = io.Discard
	}
	if poll <= 0 {
		poll = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var last Readiness
	var lastErr error
	for {
		rd, idx, err := CheckReadiness(ctx, dyn, cs, opts)
		switch {
		case err != nil:
			lastErr = err
			fmt.Fprintf(log, "readiness: %v\n", err)
		case rd.Ready:
			fmt.Fprintln(log, rd.Summary())
			return rd, idx, nil
		default:
			last, lastErr = rd, nil
			fmt.Fprintf(log, "%s: %s\n", rd.Summary(), strings.Join(rd.Problems, "; "))
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return last, nil, fmt.Errorf("readiness: %w after %s", lastErr, timeout)
			}
			return last, nil, fmt.Errorf("cluster not ready after %s: %s", timeout, strings.Join(last.Problems, "; "))
		}
		select {
		case <-ctx.Done():
			return last, nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ScanMCP indexes the cluster and returns the MCP endpoints it exposes.
func ScanMCP(ctx context.Context, dyn dynamic.Interface, cs kubernetes.Interface, opts Options) ([]MCPEndpoint, *Index, error) {
	idx, err := Scan(ctx, dyn, cs, opts)
	if err != nil {
		return nil, nil, err
	}
	return idx.MCP, idx, nil
}
