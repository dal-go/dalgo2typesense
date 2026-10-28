package dalgo2typesense

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
)

// TestHeaders_ContextIdentityBeforeHTTP proves a failing header callback keeps
// the caller's cancellation identity and never reaches the network. A provider
// error unrelated to the context must still be sanitised to ErrHeaderProvider.
func TestHeaders_ContextIdentityBeforeHTTP(t *testing.T) {
	newSearcher := func(t *testing.T, headers func(context.Context) (map[string]string, error)) (NativeSearcher, *testServer) {
		t.Helper()
		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("a header failure must not issue an HTTP request")
		})
		db := newTestDB(t, server, func(cfg *Config) { cfg.Headers = headers })
		searcher, ok := dal.As[NativeSearcher](db)
		if !ok {
			t.Fatal("NativeSearcher capability is missing")
		}
		return searcher, server
	}

	t.Run("provider-observes-caller-cancel", func(t *testing.T) {
		started := make(chan struct{})
		var calls atomic.Int32
		searcher, server := newSearcher(t, func(ctx context.Context) (map[string]string, error) {
			calls.Add(1)
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := searcher.Search(ctx, SearchRequest{Q: "*", Limit: 1})
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("header provider did not start")
		}
		cancel()
		err := waitForSearch(t, done)
		if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want ErrCanceled wrapping context.Canceled", err)
		}
		if errors.Is(err, ErrHeaderProvider) {
			t.Fatal("cancellation was sanitised to ErrHeaderProvider")
		}
		if calls.Load() != 1 {
			t.Fatalf("provider calls = %d, want 1", calls.Load())
		}
		if server.requests.Load() != 0 {
			t.Fatalf("header failure issued %d HTTP requests", server.requests.Load())
		}
	})

	t.Run("provider-observes-operation-timeout", func(t *testing.T) {
		server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("a header failure must not issue an HTTP request")
		})
		db := newTestDB(t, server, func(cfg *Config) {
			cfg.Timeout = 100 * time.Millisecond
			cfg.Headers = func(ctx context.Context) (map[string]string, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}
		})
		searcher, _ := dal.As[NativeSearcher](db)
		_, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
		if !errors.Is(err, ErrDeadlineExceeded) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want ErrDeadlineExceeded wrapping context.DeadlineExceeded", err)
		}
		if errors.Is(err, ErrHeaderProvider) {
			t.Fatal("operation timeout was sanitised to ErrHeaderProvider")
		}
		if server.requests.Load() != 0 {
			t.Fatalf("timeout issued %d HTTP requests", server.requests.Load())
		}
	})

	t.Run("wrapped-secret-context-error-is-canonical", func(t *testing.T) {
		searcher, server := newSearcher(t, func(context.Context) (map[string]string, error) {
			return nil, fmt.Errorf("X-TYPESENSE-API-KEY=super-secret: %w", context.Canceled)
		})
		_, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
		if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want ErrCanceled wrapping context.Canceled", err)
		}
		if errors.Is(err, ErrHeaderProvider) {
			t.Fatal("wrapped context error was sanitised to ErrHeaderProvider")
		}
		assertNoSecret(t, err)
		if server.requests.Load() != 0 {
			t.Fatalf("header failure issued %d HTTP requests", server.requests.Load())
		}
	})

	t.Run("context-wins-over-arbitrary-error-after-cancel", func(t *testing.T) {
		started := make(chan struct{})
		searcher, server := newSearcher(t, func(ctx context.Context) (map[string]string, error) {
			close(started)
			<-ctx.Done()
			// The context is already canceled even though this returned error
			// is not a context error and carries a secret-looking message.
			return nil, errors.New("X-TYPESENSE-API-KEY=super-secret: provider failed")
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := searcher.Search(ctx, SearchRequest{Q: "*", Limit: 1})
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("header provider did not start")
		}
		cancel()
		err := waitForSearch(t, done)
		if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want ErrCanceled after observed cancellation", err)
		}
		if errors.Is(err, ErrHeaderProvider) {
			t.Fatal("observed cancellation was sanitised to ErrHeaderProvider")
		}
		assertNoSecret(t, err)
		if server.requests.Load() != 0 {
			t.Fatalf("header failure issued %d HTTP requests", server.requests.Load())
		}
	})

	t.Run("arbitrary-error-stays-sanitised", func(t *testing.T) {
		searcher, server := newSearcher(t, func(context.Context) (map[string]string, error) {
			return nil, errors.New("X-TYPESENSE-API-KEY=super-secret: provider failed")
		})
		_, err := searcher.Search(context.Background(), SearchRequest{Q: "*", Limit: 1})
		if !errors.Is(err, ErrHeaderProvider) {
			t.Fatalf("err = %v, want ErrHeaderProvider", err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("arbitrary provider error was reported as a context error")
		}
		assertNoSecret(t, err)
		if server.requests.Load() != 0 {
			t.Fatalf("header failure issued %d HTTP requests", server.requests.Load())
		}
	})
}

func waitForSearch(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("search did not return")
		return nil
	}
}

func assertNoSecret(t *testing.T, err error) {
	t.Helper()
	text := err.Error()
	if strings.Contains(text, "super-secret") || strings.Contains(text, "X-TYPESENSE-API-KEY") {
		t.Fatalf("credential material leaked in error text: %q", text)
	}
}
