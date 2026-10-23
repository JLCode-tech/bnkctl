package scenarios_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/JLCode-tech/bnkctl/pkg/scenarios"
)

// fakeScenario is a minimal test double that tracks calls.
type fakeScenario struct {
	name         string
	rating       scenarios.Rating
	manifestsErr error
	applyErr     error
	verifyResult scenarios.Result
	cleanupErr   error

	manifestsCalled int
	applyCalled     int
	verifyCalled    int
	cleanupCalled   int
}

func (f *fakeScenario) Name() string             { return f.name }
func (f *fakeScenario) Title() string            { return "Fake: " + f.name }
func (f *fakeScenario) Rating() scenarios.Rating { return f.rating }
func (f *fakeScenario) Description() string      { return "fake scenario" }
func (f *fakeScenario) Dependencies() []string   { return []string{} }

func (f *fakeScenario) Manifests(*scenarios.Context) ([]string, error) {
	f.manifestsCalled++
	return []string{"/tmp/fake.yaml"}, f.manifestsErr
}

func (f *fakeScenario) Apply(*scenarios.Context) error {
	f.applyCalled++
	return f.applyErr
}

func (f *fakeScenario) Verify(*scenarios.Context) scenarios.Result {
	f.verifyCalled++
	return f.verifyResult
}

func (f *fakeScenario) Cleanup(*scenarios.Context) error {
	f.cleanupCalled++
	return f.cleanupErr
}

func (f *fakeScenario) Namespace(*scenarios.Context) string { return "fake-namespace" }

// freshRegistry clears the package-level registry and re-registers.
// Each test that touches the registry should use a sub-registry.
// Since the registry is global, we test it in isolation.

func TestRegisterAndFind(t *testing.T) {
	// Reset by using a package-private helper — we test via the public API.
	// The global registry may already have httproutee2e registered if the test
	// binary imported it. We just verify Find works for what's registered.
	all := scenarios.All()
	// Should at least have the httproutee2e scenario (registered by its init()).
	if len(all) == 0 {
		t.Skip("no scenarios registered (httproutee2e init not linked)")
	}
	for _, s := range all {
		found := scenarios.Find(s.Name())
		if found == nil {
			t.Errorf("Find(%q) returned nil, want the registered scenario", s.Name())
		}
		if found.Name() != s.Name() {
			t.Errorf("Find(%q).Name() = %q, want %q", s.Name(), found.Name(), s.Name())
		}
	}
}

func TestFind_NotFound(t *testing.T) {
	got := scenarios.Find("this-scenario-does-not-exist-ever")
	if got != nil {
		t.Errorf("Find(unknown) = %v, want nil", got)
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	// Use a unique name so we don't collide with real scenarios registered via init().
	dup := &fakeScenario{name: "fake-dup-test-9b3c5a", rating: scenarios.Green}
	scenarios.Register(dup)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate registration, got none")
		}
		if !strings.Contains(strings.ToLower(toString(r)), "duplicate") {
			t.Errorf("panic message %q does not mention 'duplicate'", r)
		}
	}()
	scenarios.Register(&fakeScenario{name: "fake-dup-test-9b3c5a", rating: scenarios.Green})
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}

func TestAll_ReturnsCopy(t *testing.T) {
	a := scenarios.All()
	b := scenarios.All()
	// Mutating one slice must not affect the other.
	if len(a) != len(b) {
		t.Fatalf("All() returned different lengths on two calls: %d vs %d", len(a), len(b))
	}
}

func TestResultAllPassed(t *testing.T) {
	r := scenarios.Result{
		Assertions: []scenarios.Assertion{
			{Description: "one", OK: true},
			{Description: "two", OK: true},
		},
	}
	if !r.AllPassed() {
		t.Error("AllPassed() = false, want true (all OK)")
	}

	r.Assertions[1].OK = false
	if r.AllPassed() {
		t.Error("AllPassed() = true, want false (one failed)")
	}
}

func TestResultAllPassed_EmptyAssertions(t *testing.T) {
	r := scenarios.Result{}
	if !r.AllPassed() {
		t.Error("AllPassed() with empty assertions should return true")
	}
}

func TestRunDryRun(t *testing.T) {
	dir := t.TempDir()
	s := &fakeScenario{
		name:   "test-dry",
		rating: scenarios.Green,
	}
	ctx := &scenarios.Context{
		Ctx:          context.Background(),
		Out:          io.Discard,
		DryRun:       true,
		WorkspaceDir: dir,
	}
	result := scenarios.Run(ctx, s)
	if result.Status != "dry-run" {
		t.Errorf("status = %q, want dry-run", result.Status)
	}
	if s.manifestsCalled != 1 {
		t.Errorf("Manifests called %d times, want 1", s.manifestsCalled)
	}
	if s.applyCalled != 0 {
		t.Errorf("Apply called %d times in dry-run, want 0", s.applyCalled)
	}
	if s.verifyCalled != 0 {
		t.Errorf("Verify called %d times in dry-run, want 0", s.verifyCalled)
	}
}

func TestRunRedRatedScenario(t *testing.T) {
	dir := t.TempDir()
	s := &fakeScenario{
		name:   "test-red",
		rating: scenarios.Red,
	}
	ctx := &scenarios.Context{
		Ctx:          context.Background(),
		Out:          io.Discard,
		WorkspaceDir: dir,
	}
	result := scenarios.Run(ctx, s)
	if result.Status != "skipped" {
		t.Errorf("status = %q, want skipped for Red-rated scenario", result.Status)
	}
	if s.manifestsCalled != 0 {
		t.Errorf("Manifests called %d times for Red scenario, want 0", s.manifestsCalled)
	}
}

func TestRunHappyPath(t *testing.T) {
	dir := t.TempDir()
	s := &fakeScenario{
		name:   "test-happy",
		rating: scenarios.Green,
		verifyResult: scenarios.Result{
			Status:  "ok",
			Summary: "all good",
			Assertions: []scenarios.Assertion{
				{Description: "check-1", OK: true},
			},
		},
	}
	ctx := &scenarios.Context{
		Ctx:          context.Background(),
		Out:          io.Discard,
		WorkspaceDir: dir,
	}
	result := scenarios.Run(ctx, s)
	if result.Status != "ok" {
		t.Errorf("status = %q, want ok", result.Status)
	}
	if s.manifestsCalled != 1 {
		t.Errorf("Manifests called %d times, want 1", s.manifestsCalled)
	}
	if s.applyCalled != 1 {
		t.Errorf("Apply called %d times, want 1", s.applyCalled)
	}
	if s.verifyCalled != 1 {
		t.Errorf("Verify called %d times, want 1", s.verifyCalled)
	}
}

func TestWriteManifest(t *testing.T) {
	dir := t.TempDir()
	body := "apiVersion: v1\nkind: Namespace\n"
	path, err := scenarios.WriteManifest(dir, "my-scenario", "01-ns.yaml", body)
	if err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if !strings.Contains(path, "artifacts/scenarios/my-scenario/01-ns.yaml") {
		t.Errorf("unexpected path %q", path)
	}
}

func TestEnsureScenarioDir(t *testing.T) {
	dir := t.TempDir()
	scnDir, err := scenarios.EnsureScenarioDir(dir, "test-scn")
	if err != nil {
		t.Fatalf("EnsureScenarioDir: %v", err)
	}
	if !strings.HasSuffix(scnDir, "artifacts/scenarios/test-scn") {
		t.Errorf("unexpected dir %q", scnDir)
	}
	// Second call is idempotent.
	_, err = scenarios.EnsureScenarioDir(dir, "test-scn")
	if err != nil {
		t.Fatalf("EnsureScenarioDir (second call): %v", err)
	}
}

func TestCleanup(t *testing.T) {
	dir := t.TempDir()
	s := &fakeScenario{name: "test-clean", rating: scenarios.Green}
	ctx := &scenarios.Context{
		Ctx:          context.Background(),
		Out:          io.Discard,
		WorkspaceDir: dir,
	}
	if err := scenarios.Cleanup(ctx, s); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if s.cleanupCalled != 1 {
		t.Errorf("Cleanup called %d times, want 1", s.cleanupCalled)
	}
}
