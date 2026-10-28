package dalgo2typesense

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ProvenanceSearch is the only provenance the adapter reports for a native
// search. It makes clear that the result came from the Typesense HTTP search
// API, not from a sampled document or a local recomputation.
const ProvenanceSearch = "typesense-native-search"

// NativeSearcher is the adapter-local native Typesense search capability.
// Obtain it with dal.As[NativeSearcher](db). It is deliberately not a DALgo
// core interface.
type NativeSearcher interface {
	Search(ctx context.Context, request SearchRequest) (SearchResult, error)
}

// SearchRequest is one bounded Typesense search. It has no arbitrary option
// bag, raw URL, management endpoint, multi-search, join or RPC surface.
type SearchRequest struct {
	// Collection is an existing collection. When Config.Collection is set it
	// must match it exactly; an empty value uses the configured collection.
	Collection string

	// Q is the native query text. "*" is the wildcard query.
	Q string

	// QueryBy lists full-text fields. It is required unless Q is "*".
	QueryBy []string

	// FilterBy is a bounded native Typesense filter expression. It is not a
	// URL and it is validated before any request. Currency, joins, object
	// filters and other unsupported grammar are refused.
	FilterBy string

	// SortBy lists simple application fields and directions. Typesense accepts
	// at most three. No id sort is invented.
	SortBy []SortField

	// FacetBy lists facet-enabled fields.
	FacetBy []string

	// MaxFacetValues bounds the returned facet values. Zero uses the smaller
	// of 10 and Config.MaxFacetValues; it can never exceed
	// Config.MaxFacetValues.
	MaxFacetValues int

	// Limit is the positive number of hits to return. It is bounded by
	// Config.MaxRows, Config.MaxRowsPerPage and the server's 250-hit page
	// limit. Zero is refused.
	Limit int

	// Offset is the non-negative hit offset.
	Offset int
}

// SortField is one explicit native sort term.
type SortField struct {
	Field      string
	Descending bool
}

// SearchResult is the bounded native result. Found and OutOf are server
// metadata, distinct from the hits actually returned. Partial and Complete are
// explicit: a server cap or cutoff is never presented as a clean end of data.
// A Typesense search_cutoff response indicator sets Partial and clears
// Complete even when the cutoff page returned zero hits and found is zero.
type SearchResult struct {
	Collection   string
	Hits         []SearchHit
	Facets       []Facet
	Found        int
	OutOf        int
	Page         int
	Returned     int
	SearchTimeMS int
	Partial      bool
	Complete     bool
	Truncated    bool
	Provenance   string
}

// SearchHit retains the native string ID and the losslessly decoded document.
type SearchHit struct {
	ID        string
	Document  map[string]any
	TextMatch int64
	Curated   bool
}

// Facet is a native facet result. Counts are native facet statistics, not SQL
// aggregates.
type Facet struct {
	Field  string
	Counts []FacetValue
	Stats  *FacetStats
}

// FacetValue is one native facet value count.
type FacetValue struct {
	Value       string
	Count       int
	Highlighted string
}

// FacetStats retains the native stats object when Typesense returns one. The
// numeric fields keep the exact JSON class: an integral statistic becomes int64
// or uint64 and a fractional one becomes float64. Stats are never presented as
// SQL aggregates.
type FacetStats struct {
	Min         any
	Max         any
	Sum         any
	Avg         any
	TotalValues int
}

type searchWire struct {
	Hits         json.RawMessage `json:"hits"`
	Found        json.RawMessage `json:"found"`
	OutOf        json.RawMessage `json:"out_of"`
	Page         json.RawMessage `json:"page"`
	SearchTimeMS json.RawMessage `json:"search_time_ms"`
	SearchCutoff json.RawMessage `json:"search_cutoff"`
	FacetCounts  json.RawMessage `json:"facet_counts"`
}

type segment struct {
	hits     []SearchHit
	facets   []Facet
	found    int
	outOf    int
	page     int
	searchMS int
	cutoff   bool
}

func (d *backend) Search(ctx context.Context, request SearchRequest) (SearchResult, error) {
	collection, query, err := d.nativeQuery(request)
	if err != nil {
		return SearchResult{}, err
	}
	op, err := d.newOperation(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	defer op.close()
	resp, err := d.request(op, searchPath(collection), query)
	if err != nil {
		return SearchResult{}, err
	}
	maxFacetValues := 0
	if len(request.FacetBy) > 0 {
		maxFacetValues, err = d.facetLimit(request)
		if err != nil {
			return SearchResult{}, err
		}
	}
	decoded, err := decodeSearchResponse(resp.body, request.Limit, request.Offset,
		request.FacetBy, maxFacetValues, d.cfg.MaxRowBytes)
	if err != nil {
		return SearchResult{}, err
	}
	return buildSearchResult(collection, decoded, request.Limit, request.Offset), nil
}

func (d *backend) nativeQuery(request SearchRequest) (string, url.Values, error) {
	collection := request.Collection
	if collection == "" {
		collection = d.cfg.Collection
	}
	if collection == "" {
		return "", nil, ErrCollectionNotConfigured
	}
	if err := d.validateCollection(collection); err != nil {
		return "", nil, err
	}
	if request.Q == "" {
		return "", nil, notSupported("typesense search requires a non-empty q")
	}
	if len(request.Q) > 4096 {
		return "", nil, unsafeValue("query text is too long")
	}
	query := url.Values{}
	query.Set("q", request.Q)

	if request.Q != "*" {
		if len(request.QueryBy) == 0 {
			return "", nil, notSupported("typesense text search requires query_by")
		}
	}
	if len(request.QueryBy) > d.cfg.MaxColumns {
		return "", nil, notSupported("query_by exceeds MaxColumns")
	}
	seenFields := map[string]bool{}
	for _, field := range request.QueryBy {
		if !validateFieldName(field, true) || seenFields[field] {
			return "", nil, unsafeValue("query_by field is not supported")
		}
		seenFields[field] = true
	}
	if len(request.QueryBy) > 0 {
		query.Set("query_by", strings.Join(request.QueryBy, ","))
	}

	if request.FilterBy != "" {
		if err := validateNativeFilter(request.FilterBy, d.cfg.MaxFilterBytes); err != nil {
			return "", nil, err
		}
		query.Set("filter_by", request.FilterBy)
	}

	if len(request.SortBy) > d.cfg.MaxSortFields {
		return "", nil, notSupported("sort_by exceeds the configured sort-field bound")
	}
	seenSort := map[string]bool{}
	sorts := make([]string, 0, len(request.SortBy))
	for _, sort := range request.SortBy {
		if sort.Field == "id" || !validateFieldName(sort.Field, false) || seenSort[sort.Field] {
			return "", nil, unsafeValue("sort field is not supported")
		}
		seenSort[sort.Field] = true
		direction := "asc"
		if sort.Descending {
			direction = "desc"
		}
		sorts = append(sorts, sort.Field+":"+direction)
	}
	if len(sorts) > 0 {
		query.Set("sort_by", strings.Join(sorts, ","))
	}

	if len(request.FacetBy) > d.cfg.MaxFacetFields {
		return "", nil, notSupported("facet_by exceeds MaxFacetFields")
	}
	seenFacets := map[string]bool{}
	facets := make([]string, 0, len(request.FacetBy))
	for _, field := range request.FacetBy {
		if !validateFieldName(field, true) || seenFacets[field] {
			return "", nil, unsafeValue("facet field is not supported")
		}
		seenFacets[field] = true
		facets = append(facets, field)
	}
	if len(facets) > 0 {
		query.Set("facet_by", strings.Join(facets, ","))
		maxFacetValues, err := d.facetLimit(request)
		if err != nil {
			return "", nil, err
		}
		query.Set("max_facet_values", strconv.Itoa(maxFacetValues))
	}

	if request.Limit <= 0 || request.Limit > d.cfg.MaxRows ||
		request.Limit > d.cfg.MaxRowsPerPage || request.Limit > absoluteMaxRowsPerPage {
		return "", nil, notSupported("typesense search requires a positive limit within MaxRows and MaxRowsPerPage")
	}
	if request.Offset < 0 || request.Offset > d.cfg.MaxOffset {
		return "", nil, notSupported("typesense search offset is outside the configured bound")
	}
	query.Set("limit", strconv.Itoa(request.Limit))
	if request.Offset > 0 {
		query.Set("offset", strconv.Itoa(request.Offset))
	}
	setSearchPolicy(query)
	return collection, query, nil
}

// facetLimit returns the effective max_facet_values for one request. A zero
// caller value means the documented default of 10, but it never exceeds the
// configured bound. The same value is sent to the server and used to validate
// the response, so the adapter cannot silently truncate or expose more values
// than it admitted.
func (d *backend) facetLimit(request SearchRequest) (int, error) {
	maxFacetValues := request.MaxFacetValues
	if maxFacetValues == 0 {
		maxFacetValues = 10
		if d.cfg.MaxFacetValues < maxFacetValues {
			maxFacetValues = d.cfg.MaxFacetValues
		}
	}
	if maxFacetValues < 0 || maxFacetValues > d.cfg.MaxFacetValues {
		return 0, notSupported("max facet values is outside the configured bound")
	}
	return maxFacetValues, nil
}

func buildSearchResult(collection string, decoded segment, limit, offset int) SearchResult {
	returned := len(decoded.hits)
	truncated := decoded.found > offset+returned
	partial := decoded.cutoff || (returned < limit && decoded.found > offset+returned)
	complete := !partial && !truncated && !decoded.cutoff
	return SearchResult{
		Collection:   collection,
		Hits:         decoded.hits,
		Facets:       decoded.facets,
		Found:        decoded.found,
		OutOf:        decoded.outOf,
		Page:         decoded.page,
		Returned:     returned,
		SearchTimeMS: decoded.searchMS,
		Partial:      partial,
		Complete:     complete,
		Truncated:    truncated,
		Provenance:   ProvenanceSearch,
	}
}

func decodeSearchResponse(payload []byte, limit, offset int, facetFields []string, maxFacetValues int, maxRowBytes int64) (segment, error) {
	var wire searchWire
	if err := decodeJSON(payload, &wire); err != nil {
		return segment{}, err
	}
	found, err := requiredInt(wire.Found, "search found")
	if err != nil {
		return segment{}, err
	}
	outOf, err := requiredInt(wire.OutOf, "search out_of")
	if err != nil {
		return segment{}, err
	}
	page := 0
	if wire.Page != nil {
		page, _, err = optionalInt(wire.Page, "search page")
		if err != nil {
			return segment{}, err
		}
	}
	searchMS := 0
	if wire.SearchTimeMS != nil {
		searchMS, _, err = optionalInt(wire.SearchTimeMS, "search_time_ms")
		if err != nil {
			return segment{}, err
		}
	}
	cutoff := false
	if trimmed := bytes.TrimSpace(wire.SearchCutoff); len(trimmed) > 0 {
		// The indicator is a strict boolean. Absent means the server did not
		// report a cutoff; null or any other class is malformed, never false.
		if bytes.Equal(trimmed, []byte("null")) {
			return segment{}, malformed("search_cutoff is not a boolean")
		}
		if err = decodeJSON(trimmed, &cutoff); err != nil {
			return segment{}, malformed("search_cutoff is not a boolean")
		}
	}
	rawHits, err := requiredArray(wire.Hits, "search hits")
	if err != nil {
		return segment{}, err
	}
	if limit > 0 && len(rawHits) > limit {
		return segment{}, malformed("search returned more hits than the requested limit")
	}
	hits := make([]SearchHit, 0, len(rawHits))
	seen := make(map[string]bool, len(rawHits))
	for _, rawHit := range rawHits {
		hit, err := decodeSearchHit(rawHit)
		if err != nil {
			return segment{}, err
		}
		if seen[hit.ID] {
			return segment{}, ErrDuplicateID
		}
		size, err := encodedSize(hit.Document)
		if err != nil {
			return segment{}, malformed("search hit document is not JSON-encodable")
		}
		if size > maxRowBytes {
			return segment{}, ErrRowTooLarge
		}
		seen[hit.ID] = true
		hits = append(hits, hit)
	}
	if (len(hits) > 0 && found < offset+len(hits)) || outOf < found {
		return segment{}, malformed("search counts are inconsistent with the requested offset")
	}
	facets, err := decodeFacets(wire.FacetCounts, facetFields, maxFacetValues)
	if err != nil {
		return segment{}, err
	}
	return segment{
		hits:     hits,
		facets:   facets,
		found:    found,
		outOf:    outOf,
		page:     page,
		searchMS: searchMS,
		cutoff:   cutoff,
	}, nil
}

func decodeSearchHit(raw json.RawMessage) (SearchHit, error) {
	item, err := requiredObject(raw, "search hit")
	if err != nil {
		return SearchHit{}, err
	}
	documentRaw, ok := item["document"]
	if !ok {
		return SearchHit{}, malformed("search hit has no document")
	}
	if _, err := requiredObject(documentRaw, "search hit document"); err != nil {
		return SearchHit{}, err
	}
	var documentValue map[string]any
	if err = decodeJSON(documentRaw, &documentValue); err != nil {
		return SearchHit{}, err
	}
	if documentValue == nil {
		return SearchHit{}, malformed("search hit document is not an object")
	}
	converted, err := convertJSONNumbers(documentValue)
	if err != nil {
		return SearchHit{}, err
	}
	documentValue, ok = converted.(map[string]any)
	if !ok {
		return SearchHit{}, malformed("search hit document is not an object")
	}
	idValue, ok := documentValue["id"]
	if !ok {
		return SearchHit{}, malformed("search hit document identity is missing")
	}
	id, ok := idValue.(string)
	if !ok {
		return SearchHit{}, malformed("search hit document identity is not a string")
	}
	if err = validateNativeID(id); err != nil {
		return SearchHit{}, err
	}
	hit := SearchHit{ID: id, Document: documentValue}
	if rawText, ok := item["text_match"]; ok {
		textMatch, err := requiredInt64(rawText, "search hit text_match")
		if err != nil {
			return SearchHit{}, err
		}
		hit.TextMatch = textMatch
	}
	if rawCurated, ok := item["curated"]; ok {
		curated, _, err := optionalBool(rawCurated, "search hit curated")
		if err != nil {
			return SearchHit{}, err
		}
		hit.Curated = curated
	}
	return hit, nil
}

func requiredInt64(raw json.RawMessage, what string) (int64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, malformed(what + " is missing")
	}
	var number json.Number
	if err := decodeJSON(trimmed, &number); err != nil {
		return 0, malformed(what + " is not an integer")
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, malformed(what + " is not a representable integer")
	}
	return value, nil
}

// decodeFacets validates a facet_counts array against the exact bounded facet
// request. Typesense 30.2 returns one entry for every requested facet, even
// when that facet has no values, so a missing entry is a truncated or malformed
// response rather than a legitimate omission. With no requested facets an empty
// (or absent) array is accepted, while any facet entry is refused instead of
// being exposed.
func decodeFacets(raw json.RawMessage, requested []string, maxValues int) ([]Facet, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		if len(requested) > 0 {
			return nil, malformed("facet_counts is missing")
		}
		return nil, nil
	}
	entries, err := requiredArray(trimmed, "facet_counts")
	if err != nil {
		return nil, err
	}
	if len(requested) == 0 {
		if len(entries) > 0 {
			return nil, malformed("facet_counts was not requested")
		}
		return nil, nil
	}
	if len(entries) != len(requested) {
		return nil, malformed("facet_counts does not match the requested facets")
	}
	requestedSet := make(map[string]bool, len(requested))
	for _, field := range requested {
		requestedSet[field] = true
	}
	facets := make([]Facet, 0, len(entries))
	seenFields := make(map[string]bool, len(entries))
	for _, entry := range entries {
		item, err := requiredObject(entry, "facet")
		if err != nil {
			return nil, err
		}
		field, err := requiredString(item["field_name"], "facet field_name")
		if err != nil {
			return nil, err
		}
		if !requestedSet[field] {
			return nil, malformed("facet field was not requested")
		}
		if seenFields[field] {
			return nil, malformed("duplicate facet field")
		}
		seenFields[field] = true
		rawCounts, err := requiredArray(item["counts"], "facet counts")
		if err != nil {
			return nil, err
		}
		if len(rawCounts) > maxValues {
			return nil, malformed("facet value count exceeds the requested bound")
		}
		facet := Facet{Field: field, Counts: make([]FacetValue, 0, len(rawCounts))}
		seenValues := make(map[string]bool, len(rawCounts))
		for _, rawCount := range rawCounts {
			countItem, err := requiredObject(rawCount, "facet value")
			if err != nil {
				return nil, err
			}
			value, err := requiredString(countItem["value"], "facet value")
			if err != nil {
				return nil, err
			}
			if seenValues[value] {
				return nil, malformed("duplicate facet value")
			}
			seenValues[value] = true
			count, err := requiredInt(countItem["count"], "facet count")
			if err != nil {
				return nil, err
			}
			highlighted := ""
			if rawHighlighted, ok := countItem["highlighted"]; ok {
				highlighted, err = requiredString(rawHighlighted, "facet highlighted")
				if err != nil {
					return nil, err
				}
			}
			facet.Counts = append(facet.Counts, FacetValue{Value: value, Count: count, Highlighted: highlighted})
		}
		if rawStats, ok := item["stats"]; ok && len(bytes.TrimSpace(rawStats)) > 0 && !bytes.Equal(bytes.TrimSpace(rawStats), []byte("null")) {
			stats, err := decodeFacetStats(rawStats)
			if err != nil {
				return nil, err
			}
			facet.Stats = stats
		}
		facets = append(facets, facet)
	}
	return facets, nil
}

func decodeFacetStats(raw json.RawMessage) (*FacetStats, error) {
	item, err := requiredObject(raw, "facet stats")
	if err != nil {
		return nil, err
	}
	stats := &FacetStats{}
	for _, field := range []struct {
		name   string
		target *any
	}{
		{"min", &stats.Min},
		{"max", &stats.Max},
		{"sum", &stats.Sum},
		{"avg", &stats.Avg},
	} {
		if rawValue, ok := item[field.name]; ok {
			value, present, err := optionalNumber(rawValue, "facet stats "+field.name)
			if err != nil {
				return nil, err
			}
			if present {
				*field.target = value
			}
		}
	}
	if rawTotal, ok := item["total_values"]; ok {
		total, _, err := optionalInt(rawTotal, "facet total_values")
		if err != nil {
			return nil, err
		}
		stats.TotalValues = total
	}
	return stats, nil
}

// validateNativeFilter bounds a caller-authored native filter to the
// documented Typesense filter grammar this adapter supports. It rejects joins
// ($), object filters ({}) and control characters, and requires balanced
// parentheses and backtick literals. Backticks cannot be escaped inside a
// Typesense literal, so an unbalanced backtick is rejected rather than sent.
func validateNativeFilter(filter string, maxBytes int) error {
	if filter == "" {
		return nil
	}
	if len(filter) > maxBytes {
		return ErrRequestTooLarge
	}
	if !utf8.ValidString(filter) {
		return unsafeValue("filter is not valid UTF-8")
	}
	depth := 0
	inBacktick := false
	for _, r := range filter {
		if r == 0 || r == '\r' || r == '\n' || r == 0x7f {
			return unsafeValue("filter contains a control character")
		}
		if inBacktick {
			if r == '`' {
				inBacktick = false
			}
			continue
		}
		switch r {
		case '`':
			inBacktick = true
		case '(':
			depth++
			if depth > defaultMaxDepth {
				return unsafeValue("filter nesting is too deep")
			}
		case ')':
			depth--
			if depth < 0 {
				return unsafeValue("filter has unbalanced parentheses")
			}
		case '{', '}', '$':
			return notSupported("filter uses unsupported join or object-filter syntax")
		}
	}
	if inBacktick {
		return unsafeValue("filter has an unbalanced literal")
	}
	if depth != 0 {
		return unsafeValue("filter has unbalanced parentheses")
	}
	return nil
}
