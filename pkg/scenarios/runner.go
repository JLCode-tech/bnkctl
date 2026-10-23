package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	k8sclient "github.com/JLCode-tech/bnkctl/pkg/k8s"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

const (
	scenarioSchema   = "awsbnkctl.scenario.v1"
	runSummarySchema = "awsbnkctl.scenario.run.v1"
)

// NewContext builds a Context from a kubeconfig path, parsed cluster intent,
// and loaded state. This is the single entry point used by both
// `scenarios run` and the `test traffic` alias.
func NewContext(
	ctx context.Context,
	kubeconfigPath string,
	cl *intent.Cluster,
	st *state.State,
	out io.Writer,
	dryRun bool,
	opts map[string]string,
) (*Context, error) {
	cfg, err := k8sclient.BuildRESTConfig(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("building REST config: %w", err)
	}
	cs, err := k8sclient.BuildClientset(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}
	dyn, err := k8sclient.BuildDynamicClient(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}
	if opts == nil {
		opts = make(map[string]string)
	}
	return &Context{
		Ctx:            ctx,
		Cluster:        cl,
		State:          st,
		Clientset:      cs,
		Dynamic:        dyn,
		RESTConfig:     cfg,
		KubeconfigPath: kubeconfigPath,
		WorkspaceDir:   cl.StateDir(),
		Out:            out,
		DryRun:         dryRun,
		Options:        opts,
	}, nil
}

// Run drives one scenario through Manifests → Apply → Verify and
// persists a report at <WorkspaceDir>/reports/<stamp>/scenarios/<name>.json.
// Returns the Result so the caller (CLI) can set the process exit code.
func Run(ctx *Context, s Scenario) Result {
	started := time.Now()
	fmt.Fprintf(ctx.Out, "scenario:  %s  (%s)\n", s.Name(), s.Rating())
	fmt.Fprintf(ctx.Out, "title:     %s\n\n", s.Title())

	if s.Rating() == Red {
		r := Result{
			Status:  "skipped",
			Summary: "rated red — not testable in this cluster shape",
			Details: s.Description(),
		}
		writeReport(ctx.WorkspaceDir, ctx.ReportStamp, s.Name(), r, started)
		fmt.Fprintln(ctx.Out, "SKIPPED — see report")
		return r
	}

	fmt.Fprintln(ctx.Out, "[1/3] Rendering manifests ...")
	paths, err := s.Manifests(ctx)
	if err != nil {
		return finalize(ctx, s, started, Result{
			Status:  "failed",
			Summary: "render: " + err.Error(),
		})
	}
	for _, p := range paths {
		fmt.Fprintf(ctx.Out, "      %s\n", p)
	}

	if ctx.DryRun {
		r := Result{
			Status:   "dry-run",
			Summary:  fmt.Sprintf("%d manifest(s) rendered; nothing applied", len(paths)),
			Manifest: strings.Join(paths, ","),
		}
		writeReport(ctx.WorkspaceDir, ctx.ReportStamp, s.Name(), r, started)
		return r
	}

	fmt.Fprintln(ctx.Out, "[2/3] Applying ...")
	if err := s.Apply(ctx); err != nil {
		return finalize(ctx, s, started, Result{
			Status:  "failed",
			Summary: "apply: " + err.Error(),
		})
	}

	fmt.Fprintln(ctx.Out, "[3/3] Verifying ...")
	r := s.Verify(ctx)
	r.Manifest = strings.Join(paths, ",")
	return finalize(ctx, s, started, r)
}

func finalize(ctx *Context, s Scenario, started time.Time, r Result) Result {
	// Render and embed the ASCII env diagram.
	if ctx.Cluster != nil && ctx.State != nil {
		in := EnvDiagramInput{
			Cluster:   ctx.Cluster,
			State:     ctx.State,
			Scenario:  s.Name(),
			Clientset: ctx.Clientset,
			Dynamic:   ctx.Dynamic,
			Namespace: s.Namespace(ctx),
		}
		r.EnvDiagram = Render(in)
	}
	writeReport(ctx.WorkspaceDir, ctx.ReportStamp, s.Name(), r, started)
	if ctx.Verbose && len(r.Assertions) > 0 {
		for _, a := range r.Assertions {
			mark := "✓"
			if !a.OK {
				mark = "✗"
			}
			line := fmt.Sprintf("      %s %s", mark, a.Description)
			if a.Got != "" {
				line += "  (" + a.Got + ")"
			}
			fmt.Fprintln(ctx.Out, line)
		}
	}
	fmt.Fprintf(ctx.Out, "      %s — %s\n", strings.ToUpper(r.Status), r.Summary)
	if r.Details != "" && (ctx.Verbose || r.Status == "failed") {
		fmt.Fprintln(ctx.Out, "      "+r.Details)
	}
	return r
}

// writeReport persists the result as JSON under
// <WorkspaceDir>/reports/<stamp>/scenarios/<name>.json.
// If stamp is empty, the scenario's started-at time is used.
// Errors are warned, not raised — a missing report shouldn't fail the run.
func writeReport(workspaceDir, stamp, name string, r Result, started time.Time) {
	if stamp == "" {
		stamp = started.UTC().Format("2006-01-02T15-04-05Z")
	}
	dir := filepath.Join(workspaceDir, "reports", stamp, "scenarios")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return
	}

	full := struct {
		Result
		Schema    string    `json:"schema"`
		Scenario  string    `json:"scenario"`
		StartedAt time.Time `json:"started_at"`
		Duration  string    `json:"duration"`
	}{
		Result:    r,
		Schema:    scenarioSchema,
		Scenario:  name,
		StartedAt: started.UTC(),
		Duration:  time.Since(started).Truncate(time.Second).String(),
	}
	data, _ := json.MarshalIndent(full, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, name+".json"), data, 0o600)

	// Persist env diagram separately if present.
	if r.EnvDiagram != "" {
		scnDir := filepath.Join(workspaceDir, "reports", stamp, "scenarios", name)
		if err := os.MkdirAll(scnDir, 0o750); err == nil {
			_ = os.WriteFile(filepath.Join(scnDir, "env-diagram.txt"), []byte(r.EnvDiagram), 0o600)
		}
	}
}

// Cleanup runs the scenario's Cleanup hook and emits a one-line status.
func Cleanup(ctx *Context, s Scenario) error {
	fmt.Fprintf(ctx.Out, "scenario:  %s\ncleaning...\n", s.Name())
	if err := s.Cleanup(ctx); err != nil {
		return err
	}
	fmt.Fprintln(ctx.Out, "OK")
	return nil
}

// EnsureScenarioDir creates <WorkspaceDir>/artifacts/scenarios/<name>/ and
// returns its absolute path. Scenarios use this to write rendered manifests.
func EnsureScenarioDir(workspaceDir, name string) (string, error) {
	dir := filepath.Join(workspaceDir, "artifacts", "scenarios", name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return dir, nil
}

// WriteManifest writes body to <WorkspaceDir>/artifacts/scenarios/<name>/<file>.
// Returns the absolute path written.
func WriteManifest(workspaceDir, name, file, body string) (string, error) {
	dir, err := EnsureScenarioDir(workspaceDir, name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// RunSummary is the aggregate report `--all` writes at the end of a
// multi-scenario run.
type RunSummary struct {
	Schema    string         `json:"schema"`
	StartedAt time.Time      `json:"started_at"`
	Finished  time.Time      `json:"finished_at"`
	Duration  string         `json:"duration"`
	Total     int            `json:"total"`
	Passed    int            `json:"passed"`
	Failed    int            `json:"failed"`
	Skipped   int            `json:"skipped"`
	Scenarios []SummaryEntry `json:"scenarios"`
}

// SummaryEntry is one row in RunSummary.Scenarios.
type SummaryEntry struct {
	Name     string `json:"name"`
	Rating   string `json:"rating"`
	Status   string `json:"status"`
	Duration string `json:"duration,omitempty"`
	Summary  string `json:"summary"`
}

// WriteRunSummary persists the aggregate as
// `run-<clusterName>-<stamp>.{json,md}` under <WorkspaceDir>/reports/<stamp>/.
// Returns the base filename (without extension) on success.
func WriteRunSummary(workspaceDir, clusterName, stamp string, sum RunSummary) (string, error) {
	sum.Schema = runSummarySchema
	dir := filepath.Join(workspaceDir, "reports", stamp)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	base := fmt.Sprintf("run-%s-%s", safeSlug(clusterName), stamp)
	data, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, base+".json"), data, 0o600); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# scenario run %s\n\n", stamp)
	fmt.Fprintf(&b, "- started:  %s\n", sum.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- finished: %s\n", sum.Finished.Format(time.RFC3339))
	fmt.Fprintf(&b, "- duration: %s\n", sum.Duration)
	fmt.Fprintf(&b, "- total: %d   passed: %d   failed: %d   skipped: %d\n\n",
		sum.Total, sum.Passed, sum.Failed, sum.Skipped)
	fmt.Fprintln(&b, "| Scenario | Rating | Status | Duration | Summary |")
	fmt.Fprintln(&b, "|---|---|---|---|---|")
	for _, e := range sum.Scenarios {
		dur := e.Duration
		if dur == "" {
			dur = "—"
		}
		fmt.Fprintf(&b, "| [`%s`](scenarios/%s.json) | %s | %s | %s | %s |\n",
			e.Name, e.Name, e.Rating, e.Status, dur, mdEscape(e.Summary))
	}
	return base, os.WriteFile(filepath.Join(dir, base+".md"), []byte(b.String()), 0o600)
}

func mdEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ")
}

// safeSlug strips characters that would be awkward in a filename.
// Duplicate of internal/cli/safeSlug to keep packages independent.
func safeSlug(s string) string {
	if s == "" {
		return "cluster"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// Discard is an io.Writer that drops everything — handy for tests
// that don't want scenario chatter on stderr.
var Discard io.Writer = io.Discard
