package awsmw

import (
	"context"
	"fmt"
	"testing"

	smithy "github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// fakeAPIError implements smithy.APIError for testing.
type fakeAPIError struct {
	code    string
	message string
}

func (f *fakeAPIError) ErrorCode() string             { return f.code }
func (f *fakeAPIError) ErrorMessage() string          { return f.message }
func (f *fakeAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }
func (f *fakeAPIError) Error() string {
	return fmt.Sprintf("api error %s: %s", f.code, f.message)
}

// invokeMiddleware installs WithSSOWatch on a new stack, then invokes the
// SSOWatch middleware directly by retrieving it by ID.
func invokeMiddleware(t *testing.T, handlerErr error) {
	t.Helper()
	ResetForTest()

	stack := middleware.NewStack("test", func() interface{} { return nil })
	if err := WithSSOWatch(stack); err != nil {
		t.Fatalf("WithSSOWatch registration: %v", err)
	}

	// Retrieve the middleware by its registered ID.
	mw, ok := stack.Deserialize.Get("awsbnkctl.SSOWatch")
	if !ok {
		t.Fatal("SSOWatch middleware not found in stack")
	}

	// Provide a next handler that always returns handlerErr.
	next := middleware.DeserializeHandlerFunc(
		func(_ context.Context, _ middleware.DeserializeInput) (middleware.DeserializeOutput, middleware.Metadata, error) {
			return middleware.DeserializeOutput{}, middleware.Metadata{}, handlerErr
		},
	)

	_, _, _ = mw.HandleDeserialize(context.Background(), middleware.DeserializeInput{}, next)
}

func TestWithSSOWatch_SetsFlag_ExpiredToken(t *testing.T) {
	apiErr := &fakeAPIError{code: "ExpiredToken", message: "token expired"}
	invokeMiddleware(t, apiErr)
	if !AuthFailed() {
		t.Fatal("expected authFail to be set after ExpiredToken")
	}
}

func TestWithSSOWatch_SetsFlag_InvalidClientTokenId(t *testing.T) {
	apiErr := &fakeAPIError{code: "InvalidClientTokenId", message: "bad token"}
	invokeMiddleware(t, apiErr)
	if !AuthFailed() {
		t.Fatal("expected authFail to be set after InvalidClientTokenId")
	}
}

func TestWithSSOWatch_SetsFlag_SSOSessionMessage(t *testing.T) {
	apiErr := &fakeAPIError{code: "SomeCode", message: "SSO session associated with this profile has expired"}
	invokeMiddleware(t, apiErr)
	if !AuthFailed() {
		t.Fatal("expected authFail to be set for SSO session message")
	}
}

func TestWithSSOWatch_DoesNotSetFlag_NonAuthError(t *testing.T) {
	apiErr := &fakeAPIError{code: "ThrottlingException", message: "rate exceeded"}
	invokeMiddleware(t, apiErr)
	if AuthFailed() {
		t.Fatal("authFail should not be set for a non-auth error")
	}
}

func TestWithSSOWatch_DoesNotSetFlag_NilError(t *testing.T) {
	invokeMiddleware(t, nil)
	if AuthFailed() {
		t.Fatal("authFail should not be set when no error")
	}
}

func TestCheckAuthOrDie_NoOpWhenFlagClear(t *testing.T) {
	ResetForTest()
	exited := false
	ExitFunc = func(code int) { exited = true }
	CheckAuthOrDie("my-profile")
	if exited {
		t.Fatal("CheckAuthOrDie should not exit when no auth failure")
	}
}

func TestCheckAuthOrDie_ExitsWhenFlagSet(t *testing.T) {
	ResetForTest()
	authFail.Store(true)
	exitCode := -1
	ExitFunc = func(code int) { exitCode = code }
	CheckAuthOrDie("my-profile")
	if exitCode != 99 {
		t.Fatalf("expected exit code 99, got %d", exitCode)
	}
}
