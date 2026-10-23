package scenarios

import (
	"context"
	"fmt"

	k8sapply "github.com/JLCode-tech/bnkctl/pkg/k8s"
)

// applyRunner is swappable in tests; defaults to the real SSA apply.
var applyRunner = func(ctx context.Context, ao *k8sapply.ApplyOptions) error { return ao.Run(ctx) }

// ApplyManifests server-side-applies the scenario's rendered manifest dir with
// Force=true. Force is REQUIRED so a scenario can be re-applied without first
// running `scenarios clean`: the pool-member resync (pkg/bnk.ResyncHTTPRoutes)
// JSON-patches HTTPRoute weights, leaving a managedFields entry under manager
// "awsbnkctl"/operation Update on .spec.rules; a non-forced SSA Apply (operation
// Apply, same manager) would then conflict. Scenarios own the spec they declare
// (the BNK controller owns status), so force-conflicts is correct here.
func ApplyManifests(sctx *Context, scnName string) error {
	dir, err := EnsureScenarioDir(sctx.WorkspaceDir, scnName)
	if err != nil {
		return fmt.Errorf("ensuring scenario dir: %w", err)
	}
	ao := &k8sapply.ApplyOptions{
		Filename:       dir,
		KubeconfigPath: sctx.KubeconfigPath,
		Force:          true,
	}
	return applyRunner(sctx.Ctx, ao)
}
