package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"

	computepb "cloud.google.com/go/compute/apiv1/computepb"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/jwt"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// requiredAPIs is the set of GCP services that must be enabled on the target
// project before Phase 10 / Phase 20 can succeed.
var requiredAPIs = []string{
	"compute.googleapis.com",
	"container.googleapis.com",
}

// quotaWarnRemainingPct is the threshold below which a regional quota warning
// is logged. Preflight never fails on quota — actual enforcement happens at
// resource-create time — because the exact CPU/IP ask depends on machine-type
// family and existing usage that preflight cannot perfectly predict.
const quotaWarnRemainingPct = 10.0

// Phase00Preflight verifies GCP credentials, required project APIs, and
// regional compute quotas before any mutating phase runs.
//
// When clients is nil (e.g., --dry-run without ADC, or SKIP_AUTH=true),
// live GCP calls are skipped after logging the intent.
func Phase00Preflight(ctx context.Context, cl *intent.Cluster, st *state.State, clients *Clients, dryRun bool) error {
	slog.Info("phase 00: preflight",
		"cluster", cl.Metadata.Name,
		"project", cl.Metadata.Project,
		"zone", cl.Metadata.Zone,
		"region", cl.Region,
		"pattern", cl.Pattern,
		"dry_run", dryRun,
	)

	// Validate and check the F5 JWT token if present in the intent.
	if cl.Bnk != nil && cl.Bnk.JwtPath != "" {
		rawJWT, err := os.ReadFile(cl.Bnk.JwtPath) // #nosec G304 -- operator-supplied JWT path from cluster.yaml
		if err != nil {
			if !dryRun {
				return fmt.Errorf("phase 00: read jwt file %s: %w", cl.Bnk.JwtPath, err)
			}
			slog.Warn("phase 00: could not read jwt file in dry-run", "path", cl.Bnk.JwtPath, "error", err)
		} else {
			if _, err := jwt.CheckPreflight(string(rawJWT), os.Stderr); err != nil {
				return fmt.Errorf("phase 00: invalid jwt in %s: %w", cl.Bnk.JwtPath, err)
			}
		}
	}

	if clients == nil {
		slog.Info("phase 00: skipping live GCP preflight checks (no GCP clients initialized)")
		return nil
	}

	// 1. Verify Application Default Credentials are present and readable when running live.
	if !ShouldSkipAuth() && (!testing.Testing() || os.Getenv("BNKCTL_FORCE_LIVE_AUTH") == "1") {
		if err := CheckCredentials(ctx); err != nil {
			return fmt.Errorf("phase 00: %w", err)
		}
	}

	// 2. Verify required APIs are enabled on the target project.
	if clients.ServiceUsage != nil {
		for _, api := range requiredAPIs {
			enabled, err := checkAPIEnabled(ctx, clients.ServiceUsage, cl.Metadata.Project, api)
			if err != nil {
				if isPermissionDenied(err) {
					slog.Warn("phase 00: serviceusage.services.get denied; assuming API is enabled (will surface at resource creation if not)",
						"api", api,
						"project", cl.Metadata.Project,
					)
					continue
				}
				return fmt.Errorf("phase 00: checking API %s on project %s: %w", api, cl.Metadata.Project, err)
			}
			if !enabled {
				return fmt.Errorf("phase 00: required GCP API %q is not enabled on project %q — run: gcloud services enable %s --project=%s",
					api, cl.Metadata.Project, api, cl.Metadata.Project)
			}
			slog.Info("phase 00: API enabled", "api", api)
		}
	}

	// 3. Best-effort regional quota inspection (non-fatal warning only).
	warnRegionQuota(ctx, clients.Regions, cl.Metadata.Project, cl.Region)

	_ = st // state.env is not mutated in phase 00; kept in signature for phase uniformity.
	return nil
}

// checkAPIEnabled returns true when Service Usage reports state == "ENABLED"
// for projects/<project>/services/<api>.
func checkAPIEnabled(ctx context.Context, su ServiceUsageAPI, project, api string) (bool, error) {
	name := fmt.Sprintf("projects/%s/services/%s", project, api)
	svc, err := su.GetService(ctx, name)
	if err != nil {
		return false, err
	}
	return svc.State == "ENABLED", nil
}

func isPermissionDenied(err error) bool {
	return isForbidden(err)
}

// warnRegionQuota fetches the target region's compute quotas and logs a
// warning for any metric with less than quotaWarnRemainingPct headroom.
// Errors are logged at debug level and never fail preflight.
func warnRegionQuota(ctx context.Context, regions RegionsAPI, project, region string) {
	if regions == nil || region == "" {
		return
	}
	r, err := regions.Get(ctx, &computepb.GetRegionRequest{
		Project: project,
		Region:  region,
	})
	if err != nil {
		slog.Debug("phase 00: could not inspect region quotas (non-fatal)", "region", region, "err", err)
		return
	}
	for _, q := range r.GetQuotas() {
		limit := q.GetLimit()
		usage := q.GetUsage()
		if limit <= 0 {
			continue
		}
		remainingPct := ((limit - usage) / limit) * 100.0
		if remainingPct < quotaWarnRemainingPct {
			slog.Warn("phase 00: low regional quota headroom",
				"region", region,
				"metric", q.GetMetric(),
				"usage", usage,
				"limit", limit,
				"remaining_pct", fmt.Sprintf("%.1f%%", remainingPct),
			)
		}
	}
}
