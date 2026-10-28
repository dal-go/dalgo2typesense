# dalgo2typesense

A bounded, read-only native [Typesense](https://typesense.org) adapter for
[DALgo](https://github.com/dal-go/dalgo).

It reads documents already stored in an existing Typesense 30.2 collection. It
does not write documents, create collections, or require a DALgo envelope.
The Typesense document `id` (a string, including numeric-looking values such as
`"001"`) is the DALgo record key.

## Status

Adapter checkpoint. The Go CLI and any TypeScript/web UI are separate follow-on
deliverables, not part of this module. The module does not link, embed, copy or
redistribute the Typesense server or SDK.

## Usage

```go
db, err := dalgo2typesense.NewDB(ctx, dalgo2typesense.Config{
    Endpoint:   "https://typesense.example.com:8108",
    Collection: "books",
    Headers: func(ctx context.Context) (map[string]string, error) {
        return map[string]string{"X-TYPESENSE-API-KEY": apiKey}, nil
    },
})
```

`NewDB` performs no network I/O. Use `dal.As[io.Closer]` for `Close`.
The header callback runs once per request, so a rotated scoped key is picked up
without constructing a second database. The adapter never logs or stores key
values.

### Native search

```go
searcher, ok := dal.As[dalgo2typesense.NativeSearcher](db)
result, err := searcher.Search(ctx, dalgo2typesense.SearchRequest{
    Collection: "books",
    Q:          "dune",
    QueryBy:    []string{"title", "author"},
    FilterBy:   "year:>=1965 && genre:=`science fiction`",
    SortBy:     []dalgo2typesense.SortField{{Field: "rank", Descending: true}},
    FacetBy:    []string{"genre"},
    Limit:      20,
})
```

`SearchRequest` is typed and bounded. It has no arbitrary option map, raw URL,
management endpoint, multi-search, join or RPC surface. `FilterBy` is validated
against a documented subset: joins (`$`), object filters (`{}`), control
characters, unbalanced backticks and unbalanced parentheses are refused before
any request. Facet results are native Typesense facet statistics, not SQL
aggregates.

### Structured DALgo queries

A single existing root collection, `q=*`, a positive bounded limit, a bounded
offset, top-level scalar `AND`/`OR` comparisons, projection and explicit sort
are translated. Joins, grouping, `HAVING`, subqueries, computed columns, nested
fields, wildcard projection, unsupported operators and NULL tests are refused
before I/O. `IS NULL` / `IS NOT NULL` are refused because Typesense does not
provide one uniform meaning for absent and explicit-null values.

Structured queries are not text searches. They explicitly send
`num_typos=0`, `prefix=false`, `drop_tokens_threshold=0` and
`typo_tokens_threshold=0` in addition to `q=*`. Native `Q` keeps the server's
honest typo, prefix and token-dropping semantics.

### Curation and filter safety

Every read path sets:

```
enable_curations=false
filter_curated_hits=true
```

Typesense 30.2 defaults are `enable_curations=true` and
`filter_curated_hits=false`, so a pinned or promoted document can otherwise
bypass `filter_by`. The adapter applies both flags itself and the request types
offer no way for a caller to override them. There is no pinned, promoted or
native-override bypass.

### Permissions

One scoped read-only credential callback is used for every request. A pure
`documents:search` key scoped to one collection can execute native searches and
exact-id document reads. Metadata calls made with that key fail explicitly with
`ErrMetadataForbidden`; they are never converted into an empty collection list
and never fall back to an admin credential. A key that additionally has
`collections:get` and `collections:list` can discover the real collection
catalog and schema through `dbschema.SchemaReader`. The adapter never creates
collections, keys or documents.

### Identity and value fidelity

* The native Typesense `id` is always a string. `"001"` and `1` are different.
* JSON numbers are decoded with `UseNumber` and resolved exactly before any
  value is exposed. An integral value stays integral even when it is spelled
  with an exponent or a decimal point: `9007199254740993e0` and
  `9007199254740993.0` both become exactly `9007199254740993`, not the
  float64-rounded neighbour. Integers that fit `int64` become `int64`; larger
  non-negative integers that fit `uint64` become `uint64`. An integral value
  outside those ranges is rejected, never silently substituted. Only
  non-integral values become finite `float64` values with documented IEEE754
  semantics.
* A caller-supplied `json.Number` must use a valid JSON number spelling.
  Malformed forms such as `1.`, `.1` and `01` are refused rather than
  normalised.
* Facet stats keep the exact native JSON class too: an integral `min`, `max`,
  `sum` or `avg` is `int64`/`uint64`, a fractional one is `float64`. They stay
  native Typesense facet statistics, not SQL aggregates.
* Arrays, objects, booleans, strings and null are not stringified.
* A projected DALgo query returns exactly its declared columns in declaration
  order. The native identity may be fetched as a helper but never leaks into a
  projected row. A declared but absent field is represented as `nil`.
* Recordset columns are `Column[any]` with an explicit `any` `ValueType`; no
  declared affinity coerces a value.
* Missing, null or wrong `hits` / `documents` / `fields` arrays, bad IDs,
  duplicate IDs within or across pages, mismatched offsets and integer overflow
  are malformed responses, never an empty successful result.

### Pagination and server caps

Typesense accepts at most 250 hits per page. One DALgo query may span several
pages under one operation deadline, one byte budget and one page budget. `found`
and `out_of` are server metadata, distinct from `Returned` hits. A short or
empty page that the server's own `found` count says is not the end returns
`ErrPartialResult`, so a scoped `limit_hits` is never presented as a clean EOF.
An offset at or beyond `found` is a valid empty result and is reported as EOF,
not as a malformed page.

The adapter also decodes the server's `search_cutoff` response indicator as a
strict boolean. A cutoff makes a native result explicitly `Partial` with
`Complete` false, even when the cutoff page returned zero hits and `found` is
zero. DALgo readers, `Get`, `Exists` and `GetMulti` refuse a cutoff with
`ErrPartialResult`; it is never mistaken for EOF, a missing document or a full
result. `search_cutoff_ms` is not a strict bound, facets are not bound by it and
the adapter does not send it as a hard provider deadline.

No id sort is invented. An explicit application-field sort is the caller's
choice; repeatable pages are claimed only for unchanged data with a unique sort
value. The adapter does not claim snapshot isolation under concurrent writes.

## Configuration bounds

All bounds have modest defaults and hard maxima. Notable defaults:

| Option | Default | Maximum |
| --- | --- | --- |
| `MaxRows` | 1000 | 10000 |
| `MaxRowsPerPage` | 100 | 250 |
| `MaxPages` | 20 | 100 |
| `MaxOffset` | 10000 | 1000000 |
| `MaxGetMany` | 100 | 250 |
| `MaxResponseBytes` | 4 MiB | 64 MiB |
| `MaxTotalBytes` | 16 MiB | 64 MiB |
| `MaxRowBytes` | 1 MiB | 64 MiB |
| `Timeout` | 30s | 2m |

`MaxRowsPerPage` is the physical page size of the structured reader and also
bounds one native search or one exact-id `GetMulti` batch. A larger native
request is refused before I/O; the configured bound is never silently ignored.
`MaxRowBytes` is enforced by every read path (native search, structured reader
and recordset, `Get`, `Exists`, `GetMulti`) before an over-budget document is
exposed.

Redirects are refused before a credential can reach another origin. The adapter
copies a caller-supplied `http.Client` and never mutates it. It only closes a
transport it created. Error values retain a numeric HTTP status and safe
classification, never a URL, query string, body, key or provider error text.

## Licensing boundary

The Typesense server is GPL-3.0. This package is an independent MIT-licensed
HTTP client. It does not link, embed, copy or redistribute the server or its
SDK, and it does not relicense DALgo core or any CLI.

## Verification

Unit tests use `httptest` and cover outbound curation policy, header refresh,
redirect refusal, strict response shape, projection, paging, partial results,
`search_cutoff` refusal, offset-at-or-beyond-found EOF, row and page budgets,
exact exponent/decimal integer fidelity across readers, recordsets, filters and
native facet stats, metadata refusal, cancellation, close and exact-id reads.

The integration suite starts one owned, pinned
`typesense/typesense:30.2@sha256:610f2d34b1f93d00762869da2c67736775e5798d19a2c8b91b014b8a0cc1e110`
container on a random loopback port, seeds native documents and a curation set
that promotes a document excluded by a filter, and proves the unprotected
server premise and the adapter's red-to-green filter enforcement. It also
exercises multi-page reads plus an offset at and beyond the real server's found
count, and proves the configured page and row budgets against that server.
