package dalgo2typesense

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// operation carries one caller-visible operation's deadline and cumulative
// counters. Metadata, probes and every physical page of one query share it, so
// a large result cannot reset a byte or page budget by starting a new request.
type operation struct {
	ctx    context.Context
	cancel context.CancelFunc
	stop   func() bool

	bytes    int64
	requests int
}

func (d *backend) newOperation(ctx context.Context) (*operation, error) {
	if ctx == nil {
		return nil, errors.New("typesense: context is required")
	}
	if d.closed.Load() {
		return nil, ErrClosed
	}
	opCtx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	stop := context.AfterFunc(d.root, cancel)
	return &operation{ctx: opCtx, cancel: cancel, stop: stop}, nil
}

func (o *operation) close() {
	if o == nil {
		return
	}
	if o.stop != nil {
		o.stop()
	}
	o.cancel()
}

func (o *operation) check() error {
	if err := o.ctx.Err(); err != nil {
		return safeContextError(o.ctx, err)
	}
	return nil
}

type response struct {
	status int
	body   []byte
}

// request performs exactly one bounded GET request. The credential is obtained
// anew through Config.Headers for every request, so a rotated key is picked up
// without constructing a second database or metadata credential.
func (d *backend) request(op *operation, path string, query url.Values) (response, error) {
	if err := op.check(); err != nil {
		return response{}, err
	}
	// A request whose cumulative operation budget is already spent must not
	// reach the header callback or the network. Without this early refusal a
	// subsequent physical page could still allocate its per-response probe
	// after the total budget had been exhausted.
	if op.bytes >= d.cfg.MaxTotalBytes {
		return response{}, ErrResponseTooLarge
	}
	headers, err := d.requestHeaders(op.ctx)
	if err != nil {
		return response{}, err
	}
	rawQuery := query.Encode()
	if int64(len(path)+1+len(rawQuery)) > d.cfg.MaxRequestBytes {
		return response{}, ErrRequestTooLarge
	}
	rawURL := d.endpoint + path
	if rawQuery != "" {
		rawURL += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(op.ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return response{}, ErrRequestFailed
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dalgo2typesense/"+adapterVersion)

	op.requests++
	resp, err := d.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirectRefused) {
			return response{}, ErrRedirectRefused
		}
		return response{}, safeContextError(op.ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	limit := d.cfg.MaxResponseBytes
	if limit <= 0 {
		return response{}, ErrResponseTooLarge
	}
	// Never read more than the smaller of the per-response limit and what is
	// left of the operation's total budget, plus one probe byte to detect an
	// over-budget body. Reading MaxResponseBytes+1 first would allocate the
	// whole per-response allowance even when only one total byte remained.
	if remaining := d.cfg.MaxTotalBytes - op.bytes; remaining < limit {
		limit = remaining
	}
	reader := io.LimitReader(resp.Body, limit+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return response{}, safeContextError(op.ctx, err)
	}
	if int64(len(body)) > limit {
		return response{}, ErrResponseTooLarge
	}
	op.bytes += int64(len(body))

	if resp.StatusCode != http.StatusOK {
		return response{status: resp.StatusCode}, &HTTPError{StatusCode: resp.StatusCode}
	}
	return response{status: http.StatusOK, body: body}, nil
}

func (d *backend) requestHeaders(ctx context.Context) (map[string]string, error) {
	provider := d.cfg.Headers
	if provider == nil {
		return nil, ErrHeaderProvider
	}
	raw, err := provider(ctx)
	if err != nil {
		// A header provider that observes cancellation or a deadline must keep
		// the canonical context identity; only an unrelated provider failure
		// is sanitised to ErrHeaderProvider.
		if canonical := canonicalContextError(ctx, err); canonical != nil {
			return nil, canonical
		}
		return nil, ErrHeaderProvider
	}
	headers := make(map[string]string, len(raw)+1)
	seen := make(map[string]string, len(raw))
	for name, value := range raw {
		if !headerNameSafe(name) || isReservedHeader(name) || !headerValueSafe(value) {
			return nil, unsafeValue("request header is not supported")
		}
		canonical := strings.ToLower(name)
		if existing, ok := seen[canonical]; ok && existing != name {
			return nil, unsafeValue("duplicate request header")
		}
		seen[canonical] = name
		headers[name] = value
	}
	key, ok := headers["X-TYPESENSE-API-KEY"]
	if !ok || key == "" {
		// Accept any casing but normalize to the canonical header name.
		found := false
		for name, value := range headers {
			if strings.EqualFold(name, "X-TYPESENSE-API-KEY") {
				if value == "" {
					return nil, unsafeValue("API key header is empty")
				}
				delete(headers, name)
				headers["X-TYPESENSE-API-KEY"] = value
				found = true
				break
			}
		}
		if !found {
			return nil, unsafeValue("API key header is required")
		}
	}
	return headers, nil
}

func setSearchPolicy(query url.Values) {
	// Root policy: curated and pinned documents must never bypass filter_by.
	// A caller cannot override either flag because the native request type has
	// no option bag and these values are written last.
	query.Set("enable_curations", "false")
	query.Set("filter_curated_hits", "true")
}

func setStructuredPolicy(query url.Values) {
	setSearchPolicy(query)
	// A structured DALgo query is q=* with filter/sort/projection. It is not a
	// text search: typo correction, prefix matching and token dropping must not
	// silently change its meaning. These explicit flags document that while
	// leaving q=* itself intact.
	query.Set("num_typos", "0")
	query.Set("prefix", "false")
	query.Set("drop_tokens_threshold", "0")
	query.Set("typo_tokens_threshold", "0")
}
