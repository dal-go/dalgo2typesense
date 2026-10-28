package dalgo2typesense

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// Get reads one native Typesense document by string id. It uses a bounded
// exact-id search under the same read-only credential as every other read; it
// never falls back to an admin document API or an export scan. The semantics
// are proven by the real fixture. A key whose id is not a native Typesense
// string is refused before any request.
func (d *backend) Get(ctx context.Context, rec record.Record) error {
	if err := d.validateRecordKey(rec); err != nil {
		return err
	}
	hits, err := d.lookupIDs(ctx, []string{rec.Key().ID.(string)})
	if err != nil {
		return err
	}
	key := rec.Key()
	hit, ok := hits[key.ID.(string)]
	if !ok {
		err = dal.NewErrNotFoundByKey(key, nil)
		rec.SetError(err)
		return err
	}
	if err = assignNativeDocument(rec, hit.Document); err != nil {
		return err
	}
	rec.SetError(nil)
	return nil
}

// Exists reports whether a native document exists. Typesense exposes no
// document HEAD endpoint, so this is the same bounded exact-id search with a
// limit of one rather than an admin or export request.
func (d *backend) Exists(ctx context.Context, key *record.Key) (bool, error) {
	if err := d.validateKey(key); err != nil {
		return false, err
	}
	hits, err := d.lookupIDs(ctx, []string{key.ID.(string)})
	if err != nil {
		return false, err
	}
	_, ok := hits[key.ID.(string)]
	return ok, nil
}

// GetMulti reads a bounded batch with one exact-id search. All keys are
// validated before the first request, so a malformed key or an oversized batch
// performs no I/O. The batch is bounded by MaxGetMany and by MaxRowsPerPage,
// the same per-request bound native search uses. The whole batch shares one
// deadline, one byte budget and one page. On failure the returned
// *GetMultiError names the first unread item; earlier records stay populated
// and later records are left untouched.
func (d *backend) GetMulti(ctx context.Context, records []record.Record) error {
	if ctx == nil {
		return errors.New("typesense: context is required")
	}
	if len(records) > d.cfg.MaxGetMany {
		return notSupported("GetMulti exceeds MaxGetMany")
	}
	unique := make([]string, 0, len(records))
	seen := make(map[string]bool, len(records))
	for _, rec := range records {
		if err := d.validateRecordKey(rec); err != nil {
			return err
		}
		id := rec.Key().ID.(string)
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 {
		return nil
	}
	hits, err := d.lookupIDs(ctx, unique)
	if err != nil {
		return err
	}
	for index, rec := range records {
		id := rec.Key().ID.(string)
		hit, ok := hits[id]
		if !ok {
			err = dal.NewErrNotFoundByKey(rec.Key(), nil)
			rec.SetError(err)
			return &GetMultiError{Index: index, Err: err}
		}
		if err = assignNativeDocument(rec, hit.Document); err != nil {
			return &GetMultiError{Index: index, Err: err}
		}
		rec.SetError(nil)
	}
	return nil
}

func (d *backend) lookupIDs(ctx context.Context, ids []string) (map[string]SearchHit, error) {
	if len(ids) == 0 {
		return map[string]SearchHit{}, nil
	}
	if len(ids) > d.cfg.MaxGetMany {
		return nil, notSupported("document lookup exceeds MaxGetMany")
	}
	if len(ids) > d.cfg.MaxRowsPerPage {
		return nil, notSupported("document lookup exceeds MaxRowsPerPage")
	}
	collection, err := d.collection()
	if err != nil {
		return nil, err
	}
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.ContainsRune(id, '`') {
			return nil, unsafeValue("document ID cannot be used in an exact-id filter")
		}
		quoted = append(quoted, "`"+id+"`")
	}
	op, err := d.newOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer op.close()
	query := url.Values{}
	query.Set("q", "*")
	query.Set("filter_by", "id:=["+strings.Join(quoted, ",")+"]")
	query.Set("limit", strconv.Itoa(len(ids)))
	setStructuredPolicy(query)
	resp, err := d.request(op, searchPath(collection), query)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeSearchResponse(resp.body, len(ids), 0, nil, 0, d.cfg.MaxRowBytes)
	if err != nil {
		return nil, err
	}
	if decoded.cutoff {
		// A cutoff must not be mistaken for a missing document or a full batch.
		return nil, ErrPartialResult
	}
	if decoded.found != len(decoded.hits) {
		return nil, malformed("exact-id search returned inconsistent counts")
	}
	hits := make(map[string]SearchHit, len(decoded.hits))
	for _, hit := range decoded.hits {
		hits[hit.ID] = hit
	}
	return hits, nil
}

func (d *backend) validateRecordKey(rec record.Record) error {
	if rec == nil {
		return unsafeValue("record is nil")
	}
	return d.validateKey(rec.Key())
}

func assignNativeDocument(rec record.Record, document map[string]any) error {
	if rec == nil {
		return unsafeValue("record is nil")
	}
	copied := make(map[string]any, len(document))
	for key, value := range document {
		copied[key] = value
	}
	return record.MapToData(rec.Data(), copied)
}
