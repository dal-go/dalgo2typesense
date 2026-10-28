// Package dalgo2typesense provides a bounded, read-only DALgo adapter for
// native documents in an existing Typesense 30.2 collection.
//
// Documents are read as they are stored: the Typesense string id is the DALgo
// record key and no DALgo envelope is required or written. Native searches are
// exposed through the adapter-local NativeSearcher capability, reachable with
// dal.As. Structured DALgo queries are translated to a deliberately small and
// validated subset of the Typesense search API.
//
// Every read path disables curations (enable_curations=false) and asks the
// server to apply filter_by to curated hits (filter_curated_hits=true). A
// caller cannot override either policy. The adapter never follows redirects,
// never creates collections, never writes documents and never exposes raw URLs,
// management endpoints, multi-search, joins or RPC endpoints.
//
// Server licensing: Typesense server is GPL-3.0. This package is an
// independent MIT-licensed HTTP client. It does not link, embed, copy or
// redistribute the Typesense server or its SDK.
package dalgo2typesense

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// backend is the dal.Backend over one Typesense endpoint. It is returned to
// callers only through dal.NewDB, so dal.As reaches NativeSearcher, Close and
// dbschema.SchemaReader on it.
type backend struct {
	dal.ConcurrencyAvailable

	cfg      Config
	endpoint string
	client   *http.Client
	closeNet bool

	root       context.Context
	rootCancel context.CancelFunc
	closed     atomic.Bool
	closeOnce  sync.Once
}

var _ dal.Backend = (*backend)(nil)
var _ io.Closer = (*backend)(nil)

// NewDB validates cfg and returns a sealed DALgo database. It performs no
// network request. Use dal.As[io.Closer] on the returned DB for Close.
func NewDB(ctx context.Context, cfg Config) (dal.DB, error) {
	if ctx == nil {
		return nil, errors.New("typesense: context is required")
	}
	validated, endpoint, err := validateConfig(cfg)
	if err != nil {
		return nil, err
	}
	client, closeNet := httpClient(validated.HTTPClient)
	root, rootCancel := context.WithCancel(context.Background())
	return dal.NewDB(&backend{
		cfg:        validated,
		endpoint:   endpoint,
		client:     client,
		closeNet:   closeNet,
		root:       root,
		rootCancel: rootCancel,
	}), nil
}

func httpClient(supplied *http.Client) (*http.Client, bool) {
	blockRedirects := func(*http.Request, []*http.Request) error { return ErrRedirectRefused }
	if supplied == nil {
		return &http.Client{Transport: defaultTransport(), CheckRedirect: blockRedirects}, true
	}
	client := *supplied
	client.CheckRedirect = blockRedirects
	if client.Transport == nil {
		client.Transport = defaultTransport()
		return &client, true
	}
	return &client, false
}

func defaultTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport := base.Clone()
		// A pinned endpoint must not depend on ambient proxy configuration.
		transport.Proxy = nil
		return transport
	}
	return &http.Transport{Proxy: nil}
}

func (d *backend) ID() string {
	if d.cfg.Collection == "" {
		return d.endpoint
	}
	return d.endpoint + "/" + d.cfg.Collection
}

func (d *backend) Adapter() dal.Adapter { return dal.NewAdapter(adapterName, adapterVersion) }

// Schema maps a fetched native document's string id to the DALgo record key.
// The adapter does not synthesize fields from a declared schema.
func (*backend) Schema() dal.Schema {
	return dal.NewSchema(nil, func(incomplete *record.Key, data any) (*record.Key, error) {
		if incomplete == nil {
			return nil, malformed("incomplete key is required")
		}
		var id string
		switch doc := data.(type) {
		case map[string]any:
			value, ok := doc["id"]
			if !ok {
				return nil, malformed("document identity is missing")
			}
			id, ok = value.(string)
			if !ok {
				return nil, malformed("document identity is not a string")
			}
		case *map[string]any:
			if doc == nil {
				return nil, malformed("document is nil")
			}
			value, ok := (*doc)["id"]
			if !ok {
				return nil, malformed("document identity is missing")
			}
			id, ok = value.(string)
			if !ok {
				return nil, malformed("document identity is not a string")
			}
		default:
			return incomplete, nil
		}
		if err := validateNativeID(id); err != nil {
			return nil, err
		}
		return record.NewKeyWithID(incomplete.Collection(), id), nil
	})
}

// Close is idempotent. It cancels active operations, aborts active readers on
// their next observation, and releases idle connections only for a transport
// this adapter created. A caller-supplied transport stays caller-owned.
func (d *backend) Close() error {
	d.closeOnce.Do(func() {
		d.closed.Store(true)
		d.rootCancel()
		if d.closeNet {
			if closer, ok := d.client.Transport.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
		}
	})
	return nil
}

// RunReadonlyTransaction runs a bounded read callback. Typesense has no
// stateful transaction or snapshot semantics, so the adapter does not pretend
// that a DALgo readonly transaction is one; the callback runs against the same
// stateless read surface.
func (d *backend) RunReadonlyTransaction(ctx context.Context, f dal.ROTxWorker, options ...dal.TransactionOption) error {
	if ctx == nil {
		return errors.New("typesense: context is required")
	}
	if d.closed.Load() {
		return ErrClosed
	}
	if f == nil {
		return notSupported("typesense readonly transactions require a callback")
	}
	return f(ctx, readTx{backend: d, options: dal.NewTransactionOptions(options...)})
}

// RunReadwriteTransaction refuses before any I/O. Typesense search is
// read-only and this adapter exposes no write or transactional state.
func (*backend) RunReadwriteTransaction(context.Context, dal.RWTxWorker, ...dal.TransactionOption) error {
	return notSupported("typesense adapter is read-only and has no read-write transactions")
}

type readTx struct {
	*backend
	options dal.TransactionOptions
}

func (t readTx) Options() dal.TransactionOptions { return t.options }

func (d *backend) collection() (string, error) {
	if d.cfg.Collection == "" {
		return "", ErrCollectionNotConfigured
	}
	return d.cfg.Collection, nil
}

func (d *backend) validateCollection(name string) error {
	if !reCollectionName.MatchString(name) {
		return unsafeValue("collection is not a supported name")
	}
	if d.cfg.Collection != "" && name != d.cfg.Collection {
		return notSupported("typesense requests target only the configured collection")
	}
	return nil
}

func (d *backend) validateRef(ref *dal.CollectionRef) error {
	if ref == nil {
		return notSupported("typesense collection reference is required")
	}
	if ref.Parent() != nil || ref.Schema() != "" || ref.Database() != "" || ref.Alias() != "" ||
		ref.ScanLimit() != 0 || len(ref.ScanOrders()) != 0 {
		return notSupported("typesense supports one unqualified root collection")
	}
	collection, err := d.collection()
	if err != nil {
		return err
	}
	if ref.Name() != collection {
		return notSupported("typesense queries target only the configured collection")
	}
	return nil
}

func (d *backend) validateKey(key *record.Key) error {
	if key == nil || key.Parent() != nil || key.ID == nil {
		return notSupported("typesense keys must be one root document ID")
	}
	collection, err := d.collection()
	if err != nil {
		return err
	}
	if key.Collection() != collection {
		return notSupported("typesense keys target only the configured collection")
	}
	id, ok := key.ID.(string)
	if !ok {
		return notSupported("typesense document IDs are strings")
	}
	return validateNativeID(id)
}

func validateNativeID(id string) error {
	if id == "" || len(id) > 1024 {
		return unsafeValue("document ID is not supported")
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return unsafeValue("document ID is not supported")
		}
	}
	return nil
}

// searchPath returns the escaped search endpoint for one collection. The
// collection has already been validated against the name grammar.
func searchPath(collection string) string {
	return "/collections/" + url.PathEscape(collection) + "/documents/search"
}

func collectionPath(collection string) string {
	return "/collections/" + url.PathEscape(collection)
}

func isReservedHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "host", "content-length", "transfer-encoding", "connection", "trailer",
		"upgrade", "proxy-connection", "te", "keep-alive", "accept", "content-type",
		"user-agent":
		return true
	default:
		return false
	}
}

func headerValueSafe(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\r' || c == '\n' || c == 0 || (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

func headerNameSafe(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		default:
			return false
		}
	}
	return true
}
