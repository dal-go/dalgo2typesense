package dalgo2typesense

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

type countingReadCloser struct {
	mu      sync.Mutex
	data    []byte
	pos     int
	read    int
	closed  bool
	readErr error
	errAt   int
}

func newCountingBody(data string) *countingReadCloser {
	return &countingReadCloser{data: []byte(data), errAt: -1}
}

func (b *countingReadCloser) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	limit := len(b.data)
	if b.readErr != nil && b.errAt >= 0 && b.errAt < limit {
		limit = b.errAt
	}
	if b.pos >= limit {
		if b.readErr != nil && b.pos >= b.errAt {
			return 0, b.readErr
		}
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:limit])
	b.pos += n
	b.read += n
	return n, nil
}

func (b *countingReadCloser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func (b *countingReadCloser) snapshot() (read int, closed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.read, b.closed
}

type scriptedTransport struct {
	mu     sync.Mutex
	bodies []*countingReadCloser
	calls  int
}

func (t *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	index := t.calls
	t.calls++
	if index >= len(t.bodies) {
		return nil, fmt.Errorf("unexpected HTTP request %d", index+1)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       t.bodies[index],
		Request:    req,
	}, nil
}

func (t *scriptedTransport) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func newScriptedDB(t *testing.T, transport *scriptedTransport, mutate func(*Config)) dal.DB {
	t.Helper()
	cfg := Config{
		Endpoint:   "https://typesense.example.com",
		Collection: "books",
		HTTPClient: &http.Client{Transport: transport},
		Headers: func(context.Context) (map[string]string, error) {
			return map[string]string{"X-TYPESENSE-API-KEY": "test-key"}, nil
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	db, err := NewDB(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := dal.As[io.Closer](db); ok {
			_ = closer.Close()
		}
	})
	return db
}

// TestRequest_TotalBudgetBoundsBodyConsumption reproduces the reviewer's
// scenario: MaxTotalBytes=1 with a 4 MiB per-response allowance must consume
// only the one remaining byte plus one overflow probe, not the whole MiB.
func TestRequest_TotalBudgetBoundsBodyConsumption(t *testing.T) {
	body := newCountingBody(strings.Repeat("x", 4<<20))
	transport := &scriptedTransport{bodies: []*countingReadCloser{body}}
	db := newScriptedDB(t, transport, func(cfg *Config) {
		cfg.MaxTotalBytes = 1
		cfg.MaxResponseBytes = 4 << 20
	})
	searcher, _ := dal.As[NativeSearcher](db)
	_, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	read, closed := body.snapshot()
	if read != 2 {
		t.Fatalf("body bytes read = %d, want remaining 1 + one probe byte = 2", read)
	}
	if !closed {
		t.Fatal("over-budget response body was not closed")
	}
	if transport.callCount() != 1 {
		t.Fatalf("HTTP calls = %d, want 1", transport.callCount())
	}
}

// TestRequest_ExactTotalBoundary proves an operation may consume exactly its
// total budget, and that the body is closed after a successful read.
func TestRequest_ExactTotalBoundary(t *testing.T) {
	const payload = `{"found":1,"out_of":1,"page":1,"hits":[{"document":{"id":"a"}}]}`
	body := newCountingBody(payload)
	transport := &scriptedTransport{bodies: []*countingReadCloser{body}}
	db := newScriptedDB(t, transport, func(cfg *Config) {
		cfg.MaxTotalBytes = int64(len(payload))
		cfg.MaxResponseBytes = int64(len(payload))
	})
	searcher, _ := dal.As[NativeSearcher](db)
	result, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
	if err != nil {
		t.Fatalf("Search at exact total boundary: %v", err)
	}
	if result.Returned != 1 || result.Found != 1 {
		t.Fatalf("result = %+v", result)
	}
	read, closed := body.snapshot()
	if read != len(payload) {
		t.Fatalf("body bytes read = %d, want exactly %d", read, len(payload))
	}
	if !closed {
		t.Fatal("successful response body was not closed")
	}
}

// TestRequest_OverflowProbeIsOneByte proves a body one byte over the physical
// limit is detected with exactly one probe byte and is never fully consumed.
func TestRequest_OverflowProbeIsOneByte(t *testing.T) {
	body := newCountingBody(strings.Repeat("x", 10))
	transport := &scriptedTransport{bodies: []*countingReadCloser{body}}
	db := newScriptedDB(t, transport, func(cfg *Config) {
		cfg.MaxTotalBytes = 100
		cfg.MaxResponseBytes = 8
	})
	searcher, _ := dal.As[NativeSearcher](db)
	_, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	read, closed := body.snapshot()
	if read != 9 {
		t.Fatalf("body bytes read = %d, want limit 8 + one probe byte = 9", read)
	}
	if !closed {
		t.Fatal("overflowing response body was not closed")
	}
}

// TestRequest_ReadErrorKeepsContextAndCloses proves a body read error keeps the
// safe context identity, hides the provider text, and still closes the body.
func TestRequest_ReadErrorKeepsContextAndCloses(t *testing.T) {
	body := newCountingBody(strings.Repeat("x", 100))
	body.readErr = fmt.Errorf("X-TYPESENSE-API-KEY=super-secret: %w", context.Canceled)
	body.errAt = 3
	transport := &scriptedTransport{bodies: []*countingReadCloser{body}}
	db := newScriptedDB(t, transport, nil)
	searcher, _ := dal.As[NativeSearcher](db)
	_, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrCanceled wrapping context.Canceled", err)
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "X-TYPESENSE-API-KEY") {
		t.Fatalf("credential material leaked in error text: %q", err.Error())
	}
	read, closed := body.snapshot()
	if read != 3 {
		t.Fatalf("body bytes read = %d, want 3", read)
	}
	if !closed {
		t.Fatal("failed response body was not closed")
	}
}

// TestRequest_ExhaustedTotalRefusesBeforeHeadersAndHTTP proves a page after the
// total budget is spent is refused before the header callback and the network,
// so a cumulative operation cannot reset or overspend its budget.
func TestRequest_ExhaustedTotalRefusesBeforeHeadersAndHTTP(t *testing.T) {
	const page1 = `{"found":4,"out_of":4,"page":1,"hits":[
		{"document":{"id":"a"}},{"document":{"id":"b"}}]}`
	body := newCountingBody(page1)
	transport := &scriptedTransport{bodies: []*countingReadCloser{body}}
	var headerCalls atomic.Int32
	db := newScriptedDB(t, transport, func(cfg *Config) {
		cfg.MaxRowsPerPage = 2
		cfg.MaxTotalBytes = int64(len(page1))
		cfg.MaxResponseBytes = int64(len(page1))
		cfg.Headers = func(context.Context) (map[string]string, error) {
			headerCalls.Add(1)
			return map[string]string{"X-TYPESENSE-API-KEY": "test-key"}, nil
		}
	})
	query := dal.From(dal.NewRootCollectionRef("books", "")).
		NewQuery().Limit(4).SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var ids []string
	for {
		rec, nextErr := reader.Next()
		if nextErr != nil {
			if !errors.Is(nextErr, ErrResponseTooLarge) {
				t.Fatalf("Next err = %v, want ErrResponseTooLarge", nextErr)
			}
			break
		}
		ids = append(ids, rec.Key().ID.(string))
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("ids = %v, want [a b] before the exhausted budget", ids)
	}
	if transport.callCount() != 1 {
		t.Fatalf("HTTP calls = %d, want only the first page", transport.callCount())
	}
	if headerCalls.Load() != 1 {
		t.Fatalf("header provider calls = %d, want only the first page", headerCalls.Load())
	}
	read, closed := body.snapshot()
	if read != len(page1) || !closed {
		t.Fatalf("first body read/closed = %d/%v, want %d/true", read, closed, len(page1))
	}
}

// TestRequest_PagingProbeIsBoundedByRemainingTotal proves a nearly exhausted
// operation reads at most the remaining total plus one probe byte from the next
// physical page, rather than the whole per-response allowance.
func TestRequest_PagingProbeIsBoundedByRemainingTotal(t *testing.T) {
	const page1 = `{"found":4,"out_of":4,"page":1,"hits":[
		{"document":{"id":"a"}},{"document":{"id":"b"}}]}`
	const remaining = 5
	first := newCountingBody(page1)
	second := newCountingBody(strings.Repeat("y", 100))
	transport := &scriptedTransport{bodies: []*countingReadCloser{first, second}}
	db := newScriptedDB(t, transport, func(cfg *Config) {
		cfg.MaxRowsPerPage = 2
		cfg.MaxTotalBytes = int64(len(page1)) + remaining
		cfg.MaxResponseBytes = 4 << 20
	})
	query := dal.From(dal.NewRootCollectionRef("books", "")).
		NewQuery().Limit(4).SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var ids []string
	for {
		rec, nextErr := reader.Next()
		if nextErr != nil {
			if !errors.Is(nextErr, ErrResponseTooLarge) {
				t.Fatalf("Next err = %v, want ErrResponseTooLarge", nextErr)
			}
			break
		}
		ids = append(ids, rec.Key().ID.(string))
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("ids = %v, want [a b]", ids)
	}
	if transport.callCount() != 2 {
		t.Fatalf("HTTP calls = %d, want 2", transport.callCount())
	}
	read1, closed1 := first.snapshot()
	read2, closed2 := second.snapshot()
	if read1 != len(page1) || !closed1 {
		t.Fatalf("first body read/closed = %d/%v, want %d/true", read1, closed1, len(page1))
	}
	if read2 != remaining+1 {
		t.Fatalf("second body bytes read = %d, want remaining %d + one probe byte = %d",
			read2, remaining, remaining+1)
	}
	if !closed2 {
		t.Fatal("second response body was not closed")
	}
}
