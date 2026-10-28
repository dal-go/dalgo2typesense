package dalgo2typesense

import (
	"context"
	"errors"
	"fmt"

	"github.com/dal-go/dalgo/dal"
)

// The adapter returns sentinel errors for conditions callers may need to
// distinguish without parsing text. Every message is deliberately static:
// endpoints, credentials, headers, query strings and response bodies are never
// included in an error.
var (
	// ErrRequestTooLarge reports that the encoded request exceeds the
	// configured byte budget.
	ErrRequestTooLarge = errors.New("typesense: request exceeds configured byte budget")

	// ErrResponseTooLarge reports that a response body or the retained row data
	// of one operation exceeds a configured byte budget.
	ErrResponseTooLarge = errors.New("typesense: response exceeds configured byte budget")

	// ErrRowTooLarge reports that one decoded document exceeds MaxRowBytes.
	ErrRowTooLarge = errors.New("typesense: document exceeds configured row byte budget")

	// ErrTooManyRows reports that a response or operation returned more rows
	// than the configured row budget.
	ErrTooManyRows = errors.New("typesense: result exceeds configured row budget")

	// ErrTooManyPages reports that completing an operation would need more
	// search pages than the configured page budget.
	ErrTooManyPages = errors.New("typesense: result exceeds configured page budget")

	// ErrMalformedResponse reports a response that is not the expected
	// Typesense JSON shape. It never includes response text.
	ErrMalformedResponse = errors.New("typesense: malformed response")

	// ErrUnsafeValue reports an identifier, field, header or constant that the
	// adapter will not send. It never includes the rejected value.
	ErrUnsafeValue = errors.New("typesense: unsafe value")

	// ErrPartialResult reports that the server returned fewer hits than the
	// response's own found count says are available. It is used for caps such
	// as a scoped limit_hits and for search cutoffs, so a capped result is
	// never presented as a clean end of data.
	ErrPartialResult = errors.New("typesense: partial result")

	// ErrDuplicateID reports that a paged search repeated a native document ID.
	// It prevents duplicate rows silently entering a result and detects an
	// unstable page boundary.
	ErrDuplicateID = errors.New("typesense: duplicate document ID across pages")

	// ErrMetadataForbidden reports that a metadata request was refused by the
	// credential. It is never converted into an empty collection list.
	ErrMetadataForbidden = errors.New("typesense: metadata is not permitted by this credential")

	// ErrPermissionDenied reports an HTTP 401 or 403 response. The numeric
	// status remains available through *HTTPError.
	ErrPermissionDenied = errors.New("typesense: request is not permitted")

	// ErrRedirectRefused reports that the server attempted a redirect. The
	// adapter never follows a redirect because a credential must not reach a
	// different origin.
	ErrRedirectRefused = errors.New("typesense: redirect refused")

	// ErrHeaderProvider reports that the configured header provider failed.
	// The provider's own error text is not returned because it may contain a
	// credential.
	ErrHeaderProvider = errors.New("typesense: header provider failed")

	// ErrRequestFailed is the safe, detail-free error returned for a transport
	// failure that is neither a context cancellation nor a deadline.
	ErrRequestFailed = errors.New("typesense: request failed")

	// ErrClosed reports that the adapter has been closed.
	ErrClosed = errors.New("typesense: adapter is closed")

	// ErrCollectionNotConfigured reports a structured query or configured
	// metadata request on an adapter without a configured collection.
	ErrCollectionNotConfigured = errors.New("typesense: no collection is configured")

	// ErrSemanticMismatch reports that a native Typesense candidate does not
	// satisfy the exact admitted DALgo predicate. Native string folding and
	// array matching make filter_by a candidate producer, never the final
	// semantic authority; such a candidate is refused instead of silently
	// dropped, exposed or retried around. It also unwraps to
	// dal.ErrNotSupported because the adapter honestly cannot produce the
	// requested exact result under the existing bounds.
	ErrSemanticMismatch = errors.New("typesense: candidate does not satisfy the exact query semantics")
)

// HTTPError is a safe Typesense HTTP status error. Only the numeric status is
// retained; response headers and bodies are discarded.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("typesense: request failed with HTTP %d", e.StatusCode)
}

// Unwrap classifies authentication and authorization failures without
// retaining provider text.
func (e *HTTPError) Unwrap() error {
	if e != nil && (e.StatusCode == 401 || e.StatusCode == 403) {
		return ErrPermissionDenied
	}
	return nil
}

// GetMultiError reports which item of a GetMulti batch stopped the batch.
// Records before Index are populated; the record at Index and all later
// records are not. It unwraps to the underlying cause, so errors.Is and
// errors.As keep working for sentinels and record.ErrRecordNotFound.
type GetMultiError struct {
	Index int
	Err   error
}

func (e *GetMultiError) Error() string {
	if e == nil || e.Err == nil {
		return "typesense: get multi failed"
	}
	return fmt.Sprintf("typesense: get multi item %d: %v", e.Index, e.Err)
}

func (e *GetMultiError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ErrCanceled and ErrDeadlineExceeded preserve errors.Is semantics for
// context.Canceled and context.DeadlineExceeded without exposing the wrapped
// transport error, whose text may contain the endpoint or credentials.
var (
	ErrCanceled         = fmt.Errorf("typesense: request canceled: %w", context.Canceled)
	ErrDeadlineExceeded = fmt.Errorf("typesense: request deadline exceeded: %w", context.DeadlineExceeded)
)

func notSupported(reason string) error {
	return fmt.Errorf("%w: %s", dal.ErrNotSupported, reason)
}

// semanticMismatch is a typed unsupported refusal: the request itself is
// admitted, but a real candidate proves that the native filter's broader
// string/array semantics cannot satisfy it exactly within the configured
// bounds. It never names a document or a field value.
func semanticMismatch(reason string) error {
	return fmt.Errorf("%w: %w: %s", dal.ErrNotSupported, ErrSemanticMismatch, reason)
}

func unsafeValue(reason string) error {
	return fmt.Errorf("%w: %s", ErrUnsafeValue, reason)
}

func malformed(reason string) error {
	return fmt.Errorf("%w: %s", ErrMalformedResponse, reason)
}

// canonicalContextError returns a safe canonical context sentinel when either
// the supplied error or the context identifies a cancellation or deadline. The
// original error is never returned: its text may wrap a credential. A nil
// result means neither source reports a context condition.
func canonicalContextError(ctx context.Context, err error) error {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return ErrCanceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return ErrDeadlineExceeded
		}
	}
	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.Canceled) {
				return ErrCanceled
			}
			return ErrDeadlineExceeded
		}
	}
	return nil
}

// safeContextError maps a transport or reader error to a context sentinel when
// either the error or the context says the operation was canceled or timed out.
// Any other error is replaced by the detail-free ErrRequestFailed.
func safeContextError(ctx context.Context, err error) error {
	if canonical := canonicalContextError(ctx, err); canonical != nil {
		return canonical
	}
	return ErrRequestFailed
}
