package dalgo2typesense

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	adapterName    = "typesense"
	adapterVersion = "0.1.0"

	defaultMaxRows          = 1000
	defaultMaxRowsPerPage   = 100
	defaultMaxPages         = 20
	defaultMaxOffset        = 10000
	defaultMaxGetMany       = 100
	defaultMaxRequestBytes  = 64 << 10
	defaultMaxResponseBytes = 4 << 20
	defaultMaxTotalBytes    = 16 << 20
	defaultMaxRowBytes      = 1 << 20
	defaultMaxFilterBytes   = 8 << 10
	defaultMaxFacetFields   = 5
	defaultMaxFacetValues   = 100
	defaultMaxColumns       = 100
	defaultMaxSortFields    = 3
	defaultMaxDepth         = 8
	defaultMaxConditions    = 64
	defaultTimeout          = 30 * time.Second

	absoluteMaxRows          = 10000
	absoluteMaxRowsPerPage   = 250
	absoluteMaxPages         = 100
	absoluteMaxOffset        = 1_000_000
	absoluteMaxGetMany       = 250
	absoluteMaxRequestBytes  = 1 << 20
	absoluteMaxResponseBytes = 64 << 20
	absoluteMaxTotalBytes    = 64 << 20
	absoluteMaxRowBytes      = 64 << 20
	absoluteMaxFilterBytes   = 64 << 10
	absoluteMaxFacetFields   = 10
	absoluteMaxFacetValues   = 250
	absoluteMaxColumns       = 250
	absoluteMaxDepth         = 8
	absoluteMaxConditions    = 256
	absoluteMaxSortFields    = 3
	absoluteMaxTimeout       = 2 * time.Minute
)

var (
	reFieldName      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._]{0,127}$`)
	reCollectionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

// Config configures one adapter instance. The adapter addresses an existing
// Typesense server; it never creates collections or writes documents.
type Config struct {
	// Endpoint is the HTTPS origin of the Typesense server, for example
	// https://typesense.example.com:8108. It must not contain credentials, a
	// path, a query or a fragment. Plain HTTP is accepted only for a loopback
	// host when AllowLoopbackHTTP is true.
	Endpoint string

	// Collection optionally pins every request to one existing collection.
	// When empty, native searches may name any collection the credential can
	// read, while structured DALgo queries and configured metadata are refused
	// because there is no configured target.
	Collection string

	// Headers is called for every HTTP request and returns that request's
	// credential headers (typically X-TYPESENSE-API-KEY). It is the only
	// credential source. Names and values must be CR/LF/NUL-safe; hop-by-hop,
	// protocol and framing headers are refused. Provider error text is never
	// returned to the caller.
	Headers func(ctx context.Context) (map[string]string, error)

	// HTTPClient optionally supplies the client. The adapter copies it, blocks
	// redirects on the copy, and never mutates the caller's client.
	HTTPClient *http.Client

	// AllowLoopbackHTTP permits plain HTTP only for localhost, 127.0.0.1 and
	// ::1. It is a development option; production endpoints must use HTTPS.
	AllowLoopbackHTTP bool

	// MaxRows is the largest accepted DALgo query or native search limit.
	// Default 1000. Native search is one page, so it is additionally bounded by
	// MaxRowsPerPage and the server's 250-hit page limit.
	MaxRows int
	// MaxRowsPerPage bounds one search request. Default 100, hard maximum 250.
	// The structured reader uses it as its physical page size. Native search
	// and exact-id GetMulti batches refuse a request larger than this bound
	// instead of silently ignoring it.
	MaxRowsPerPage int
	// MaxPages bounds how many search requests one DALgo query may issue.
	// Default 20.
	MaxPages int
	// MaxOffset is the largest accepted offset. Default 10000.
	MaxOffset int
	// MaxGetMany is the largest accepted GetMulti batch. Default 100.
	MaxGetMany int
	// MaxRequestBytes bounds the encoded search query sent to the server.
	// Default 64 KiB.
	MaxRequestBytes int64
	// MaxResponseBytes bounds one HTTP response body. Default 4 MiB.
	MaxResponseBytes int64
	// MaxTotalBytes bounds the cumulative response bytes of one operation,
	// including metadata, paging and batches. Default 16 MiB.
	MaxTotalBytes int64
	// MaxRowBytes bounds one decoded native document's JSON size. Every read
	// path enforces it before exposing the row: the structured reader, native
	// search, Get, Exists and GetMulti. Default 1 MiB.
	MaxRowBytes int64
	// MaxFilterBytes bounds a native FilterBy expression and a compiled DALgo
	// filter. Default 8 KiB.
	MaxFilterBytes int
	// MaxFacetFields bounds the number of facet_by fields. Default 5.
	MaxFacetFields int
	// MaxFacetValues bounds max_facet_values. Default 100.
	MaxFacetValues int
	// MaxColumns bounds a structured projection and a query_by field list.
	// Default 100.
	MaxColumns int
	// MaxSortFields bounds sort fields. Typesense accepts at most 3. Default 3.
	MaxSortFields int
	// Timeout bounds one operation, including every page and request in it.
	// Default 30s.
	Timeout time.Duration
}

func validateConfig(cfg Config) (Config, string, error) {
	endpoint, err := validateEndpoint(cfg.Endpoint, cfg.AllowLoopbackHTTP)
	if err != nil {
		return Config{}, "", err
	}
	if cfg.Collection != "" && !reCollectionName.MatchString(cfg.Collection) {
		return Config{}, "", errors.New("typesense: Collection is not a supported name")
	}
	if cfg.Headers == nil {
		return Config{}, "", errors.New("typesense: Headers is required")
	}
	if cfg.MaxRows, err = boundedPositive(cfg.MaxRows, defaultMaxRows, absoluteMaxRows, "MaxRows"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxRowsPerPage, err = boundedPositive(cfg.MaxRowsPerPage, defaultMaxRowsPerPage, absoluteMaxRowsPerPage, "MaxRowsPerPage"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxPages, err = boundedPositive(cfg.MaxPages, defaultMaxPages, absoluteMaxPages, "MaxPages"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxOffset, err = boundedNonNegative(cfg.MaxOffset, defaultMaxOffset, absoluteMaxOffset, "MaxOffset"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxGetMany, err = boundedPositive(cfg.MaxGetMany, defaultMaxGetMany, absoluteMaxGetMany, "MaxGetMany"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxRequestBytes, err = boundedBytes(cfg.MaxRequestBytes, defaultMaxRequestBytes, absoluteMaxRequestBytes, "MaxRequestBytes"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxResponseBytes, err = boundedBytes(cfg.MaxResponseBytes, defaultMaxResponseBytes, absoluteMaxResponseBytes, "MaxResponseBytes"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxTotalBytes, err = boundedBytes(cfg.MaxTotalBytes, defaultMaxTotalBytes, absoluteMaxTotalBytes, "MaxTotalBytes"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxRowBytes, err = boundedBytes(cfg.MaxRowBytes, defaultMaxRowBytes, absoluteMaxRowBytes, "MaxRowBytes"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxFilterBytes, err = boundedPositive(cfg.MaxFilterBytes, defaultMaxFilterBytes, absoluteMaxFilterBytes, "MaxFilterBytes"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxFacetFields, err = boundedPositive(cfg.MaxFacetFields, defaultMaxFacetFields, absoluteMaxFacetFields, "MaxFacetFields"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxFacetValues, err = boundedPositive(cfg.MaxFacetValues, defaultMaxFacetValues, absoluteMaxFacetValues, "MaxFacetValues"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxColumns, err = boundedPositive(cfg.MaxColumns, defaultMaxColumns, absoluteMaxColumns, "MaxColumns"); err != nil {
		return Config{}, "", err
	}
	if cfg.MaxSortFields, err = boundedPositive(cfg.MaxSortFields, defaultMaxSortFields, defaultMaxSortFields, "MaxSortFields"); err != nil {
		return Config{}, "", err
	}
	if cfg.Timeout < 0 {
		return Config{}, "", errors.New("typesense: Timeout must not be negative")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Timeout > absoluteMaxTimeout {
		return Config{}, "", errors.New("typesense: Timeout exceeds the supported maximum")
	}
	return cfg, endpoint, nil
}

func validateEndpoint(raw string, allowLoopbackHTTP bool) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("typesense: Endpoint is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("typesense: Endpoint is not a valid URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if u.Opaque != "" || u.Host == "" {
		return "", errors.New("typesense: Endpoint must be an absolute URL with a host")
	}
	if u.User != nil {
		return "", errors.New("typesense: Endpoint must not contain credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("typesense: Endpoint must not contain a query or a fragment")
	}
	if scheme != "https" && scheme != "http" {
		return "", errors.New("typesense: Endpoint must use HTTPS")
	}
	if scheme == "http" {
		if !allowLoopbackHTTP {
			return "", errors.New("typesense: plain HTTP requires AllowLoopbackHTTP")
		}
		if !isLoopbackHost(u.Hostname()) {
			return "", errors.New("typesense: plain HTTP is allowed only for a loopback host")
		}
	}
	path := strings.TrimSuffix(u.Path, "/")
	if path != "" {
		return "", errors.New("typesense: Endpoint must not contain a path")
	}
	return (&url.URL{Scheme: scheme, Host: u.Host}).String(), nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func boundedPositive(value, fallback, maximum int, name string) (int, error) {
	if value < 0 {
		return 0, errors.New("typesense: " + name + " must not be negative")
	}
	if value == 0 {
		return fallback, nil
	}
	if value > maximum {
		return 0, errors.New("typesense: " + name + " exceeds the supported maximum")
	}
	return value, nil
}

func boundedNonNegative(value, fallback, maximum int, name string) (int, error) {
	if value < 0 {
		return 0, errors.New("typesense: " + name + " must not be negative")
	}
	if value == 0 {
		return fallback, nil
	}
	if value > maximum {
		return 0, errors.New("typesense: " + name + " exceeds the supported maximum")
	}
	return value, nil
}

func boundedBytes(value, fallback, maximum int64, name string) (int64, error) {
	if value < 0 {
		return 0, errors.New("typesense: " + name + " must not be negative")
	}
	if value == 0 {
		return fallback, nil
	}
	if value > maximum {
		return 0, errors.New("typesense: " + name + " exceeds the supported maximum")
	}
	return value, nil
}

// validateFieldName accepts a simple top-level field name. Dotted names are
// accepted only where Typesense documents them (query_by and facet_by) and are
// never accepted by the structured DALgo translator.
func validateFieldName(name string, allowNested bool) bool {
	if !reFieldName.MatchString(name) || strings.HasPrefix(name, "_") {
		return false
	}
	if !allowNested && strings.Contains(name, ".") {
		return false
	}
	return true
}
