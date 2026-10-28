package dalgo2typesense

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// stepContext is a deterministic context whose Err flips after a fixed number
// of observations. It lets a test cancel in the middle of one exact IN
// verification without sleeps, a blocked request or a large array.
type stepContext struct {
	context.Context
	mu    sync.Mutex
	after int
	calls int
	err   error
	done  chan struct{}
}

func newStepContext(after int, err error) *stepContext {
	return &stepContext{
		Context: context.Background(),
		after:   after,
		err:     err,
		done:    make(chan struct{}),
	}
}

func (c *stepContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls < c.after {
		return nil
	}
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return c.err
}

func (c *stepContext) Done() <-chan struct{} { return c.done }

func (c *stepContext) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// flipContext is a context whose context state is failed explicitly at the end
// of a test-controlled verification.
type flipContext struct {
	context.Context
	mu   sync.Mutex
	err  error
	done chan struct{}
}

func newFlipContext() *flipContext {
	return &flipContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *flipContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *flipContext) Done() <-chan struct{} { return c.done }

func (c *flipContext) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
}

// TestStructuredSemantics_VerifierObservesContextInsideIn proves the exact IN
// verifier asks the operation context for every literal and array element, so a
// cancellation or deadline is observed while the real comparison would
// otherwise continue to a later match.
func TestStructuredSemantics_VerifierObservesContextInsideIn(t *testing.T) {
	condition := dal.Comparison{
		Left:     dal.Field("label"),
		Operator: dal.In,
		Right:    dal.Array{Value: []string{"one", "two", "match"}},
	}
	_, verifier, err := compileConditionWithVerifier(condition, 0, new(int), 1024, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	for _, tc := range []struct {
		name  string
		value any
		after int
	}{
		// The third literal would match. Cancellation lands after the first two
		// literal comparisons, inside matchIn.
		{name: "scalar-literals", value: "match", after: 4},
		// Conversion checks three elements first; cancellation then lands after
		// the first array element was compared against all literals, still
		// inside matchIn.
		{name: "array-elements", value: []any{"x", "y", "match"}, after: 8},
		// Cancellation lands while the eager array conversion is still copying.
		{name: "array-conversion", value: []any{"x", "y", "match"}, after: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := verifier.match(context.Background(), map[string]any{"label": tc.value})
			if err != nil || !ok {
				t.Fatalf("baseline match = %v, %v; want true, nil", ok, err)
			}
			for _, cause := range []struct {
				name  string
				cause error
				want  error
			}{
				{name: "canceled", cause: context.Canceled, want: ErrCanceled},
				{name: "deadline", cause: context.DeadlineExceeded, want: ErrDeadlineExceeded},
			} {
				t.Run(cause.name, func(t *testing.T) {
					ctx := newStepContext(tc.after, cause.cause)
					ok, err := verifier.match(ctx, map[string]any{"label": tc.value})
					if ok {
						t.Fatal("canceled comparison still reported a match")
					}
					if !errors.Is(err, cause.cause) {
						t.Fatalf("match err = %v, want context cause %v", err, cause.cause)
					}
					if !errors.Is(err, cause.want) {
						t.Fatalf("match err = %v, want canonical %v", err, cause.want)
					}
					if ctx.callCount() < 3 {
						t.Fatalf("context observed only %d times, want inner-loop observation", ctx.callCount())
					}
				})
			}
		})
	}
}

// cancelAtEndVerifier runs the real exact verifier and then fails the operation
// context before returning, modelling a cancellation or deadline that arrives
// during the last exact comparison of the buffered window.
type cancelAtEndVerifier struct {
	inner  conditionVerifier
	cancel func()
}

func (v cancelAtEndVerifier) match(ctx context.Context, data map[string]any) (bool, error) {
	ok, err := v.inner.match(ctx, data)
	if err == nil {
		v.cancel()
	}
	return ok, err
}

// TestStructuredSemantics_ContextFailureAfterVerificationOutranksAdmission
// proves a canonical cancellation or deadline observed at the end of exact
// verification wins over both a mismatch refusal and admission of a verified
// row, for the false and true verifier outcomes.
func TestStructuredSemantics_ContextFailureAfterVerificationOutranksAdmission(t *testing.T) {
	condition := dal.Comparison{
		Left:     dal.Field("label"),
		Operator: dal.In,
		Right:    dal.Array{Value: []string{"classic"}},
	}
	for _, tc := range []struct {
		name  string
		label string
		cause error
		want  error
	}{
		{name: "mismatch-canceled", label: "Classic", cause: context.Canceled, want: ErrCanceled},
		{name: "verified-canceled", label: "classic", cause: context.Canceled, want: ErrCanceled},
		{name: "mismatch-deadline", label: "Classic", cause: context.DeadlineExceeded, want: ErrDeadlineExceeded},
		{name: "verified-deadline", label: "classic", cause: context.DeadlineExceeded, want: ErrDeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusOK, `{"found":1,"out_of":1,"page":1,"hits":[
					{"document":{"id":"a","label":"`+tc.label+`"}}]}`)
			})
			db := newTestDB(t, server, nil)
			be, ok := dal.As[*backend](db)
			if !ok {
				t.Fatal("test DB does not expose the adapter backend")
			}
			plan, err := be.makePlan(structuredBooksQuery(condition, 1, 0))
			if err != nil {
				t.Fatalf("makePlan: %v", err)
			}
			ctx := newFlipContext()
			plan.verifier = cancelAtEndVerifier{
				inner:  plan.verifier,
				cancel: func() { ctx.fail(tc.cause) },
			}
			reader := &recordsReader{
				op:   &operation{ctx: ctx, cancel: func() {}},
				db:   be,
				plan: plan,
				seen: map[string]bool{},
			}

			err = reader.bufferFiltered()
			if !errors.Is(err, tc.cause) {
				t.Fatalf("bufferFiltered err = %v, want context cause %v", err, tc.cause)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("bufferFiltered err = %v, want canonical %v", err, tc.want)
			}
			if errors.Is(err, ErrSemanticMismatch) {
				t.Fatal("context failure leaked as a semantic mismatch")
			}
			if len(reader.page) != 0 || reader.buffered {
				t.Fatalf("canceled verification admitted %d rows (buffered=%v)", len(reader.page), reader.buffered)
			}
			if requests := server.requests.Load(); requests != 1 {
				t.Fatalf("requests = %d, want the single buffered page", requests)
			}

			if err = reader.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if _, err = reader.Next(); !errors.Is(err, dal.ErrReaderClosed) {
				t.Fatalf("Next after Close = %v, want ErrReaderClosed", err)
			}
		})
	}
}
