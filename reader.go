package dalgo2typesense

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"
)

// ExecuteQueryToRecordsReader returns a lazily paged reader over native
// Typesense documents. Every page shares one operation deadline, one cumulative
// response-byte budget and one page budget. A page that the server cannot fill
// even though its own found count promises more is returned as ErrPartialResult,
// never as a clean EOF.
func (d *backend) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	if ctx == nil {
		return nil, errors.New("typesense: context is required")
	}
	p, err := d.makePlan(query)
	if err != nil {
		return nil, err
	}
	return d.newRecordsReader(ctx, p)
}

func (d *backend) newRecordsReader(ctx context.Context, p searchPlan) (*recordsReader, error) {
	op, err := d.newOperation(ctx)
	if err != nil {
		return nil, err
	}
	return &recordsReader{op: op, db: d, plan: p, seen: map[string]bool{}}, nil
}

type recordsReader struct {
	op   *operation
	db   *backend
	plan searchPlan

	page     []record.Record
	pos      int
	total    int
	pages    int
	seen     map[string]bool
	terminal error
	closed   bool
}

var _ dal.RecordsReader = (*recordsReader)(nil)

func (r *recordsReader) Next() (record.Record, error) {
	if r.closed {
		return nil, dal.ErrReaderClosed
	}
	if r.terminal != nil {
		return nil, r.terminal
	}
	if err := r.op.check(); err != nil {
		return r.fail(safeContextError(r.op.ctx, err))
	}
	if r.pos < len(r.page) {
		rec := r.page[r.pos]
		r.pos++
		return rec, nil
	}
	if r.total >= r.plan.limit {
		return r.eof()
	}
	if r.pages >= r.db.cfg.MaxPages {
		return r.fail(ErrTooManyPages)
	}
	pageSize := r.plan.pageSize
	if remaining := r.plan.limit - r.total; remaining < pageSize {
		pageSize = remaining
	}
	offset := r.plan.offset + r.total
	query := url.Values{}
	query.Set("q", "*")
	if r.plan.filter != "" {
		query.Set("filter_by", r.plan.filter)
	}
	if len(r.plan.sort) > 0 {
		sorts := make([]string, 0, len(r.plan.sort))
		for _, field := range r.plan.sort {
			direction := "asc"
			if field.Descending {
				direction = "desc"
			}
			sorts = append(sorts, field.Field+":"+direction)
		}
		query.Set("sort_by", strings.Join(sorts, ","))
	}
	if r.plan.hasProjection {
		query.Set("include_fields", strings.Join(r.plan.fetch, ","))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	query.Set("limit", strconv.Itoa(pageSize))
	setStructuredPolicy(query)

	resp, err := r.db.request(r.op, searchPath(r.plan.collection), query)
	if err != nil {
		return r.fail(err)
	}
	decoded, err := decodeSearchResponse(resp.body, pageSize, offset, nil, 0, r.db.cfg.MaxRowBytes)
	if err != nil {
		return r.fail(err)
	}
	r.pages++
	if decoded.cutoff {
		// A cutoff is a partial refusal, never an end of data. It is reported
		// even when the cutoff page itself returned zero hits and found is 0.
		return r.fail(ErrPartialResult)
	}
	if len(decoded.hits) == 0 {
		// An offset at or beyond the server's found count is a valid empty
		// result. Only a found count that still advertises hits after this
		// offset is a partial result.
		if decoded.found > offset {
			return r.fail(ErrPartialResult)
		}
		return r.eof()
	}
	converted := make([]record.Record, 0, len(decoded.hits))
	for _, hit := range decoded.hits {
		if r.seen[hit.ID] {
			return r.fail(ErrDuplicateID)
		}
		data, err := projectedDocument(hit, r.plan)
		if err != nil {
			return r.fail(err)
		}
		size, err := encodedSize(data)
		if err != nil {
			return r.fail(malformed("document is not JSON-encodable"))
		}
		if size > r.db.cfg.MaxRowBytes {
			return r.fail(ErrRowTooLarge)
		}
		r.seen[hit.ID] = true
		converted = append(converted, record.NewRecordWithData(record.NewKeyWithID(r.plan.collection, hit.ID), data))
	}
	if len(converted) == 0 {
		if decoded.found > offset {
			return r.fail(ErrPartialResult)
		}
		return r.eof()
	}
	if len(converted) < pageSize && decoded.found > offset+len(converted) {
		return r.fail(ErrPartialResult)
	}
	r.total += len(converted)
	r.page = converted
	r.pos = 0
	rec := r.page[0]
	r.pos = 1
	return rec, nil
}

func (r *recordsReader) Cursor() (string, error) {
	if r.closed {
		return "", dal.ErrReaderClosed
	}
	return "", notSupported("typesense readers do not issue cursors")
}

func (r *recordsReader) Close() error {
	if !r.closed {
		r.closed = true
		r.page = nil
		r.pos = 0
		r.op.close()
	}
	return nil
}

func (r *recordsReader) eof() (record.Record, error) {
	r.terminal = dal.ErrNoMoreRecords
	r.page = nil
	r.pos = 0
	r.op.close()
	return nil, r.terminal
}

func (r *recordsReader) fail(err error) (record.Record, error) {
	r.terminal = err
	r.page = nil
	r.pos = 0
	r.op.close()
	return nil, err
}

func projectedDocument(hit SearchHit, p searchPlan) (map[string]any, error) {
	if !p.hasProjection {
		data := make(map[string]any, len(hit.Document))
		for key, value := range hit.Document {
			data[key] = value
		}
		return data, nil
	}
	data := make(map[string]any, len(p.columns))
	for _, column := range p.columns {
		if column == "id" {
			data[column] = hit.ID
			continue
		}
		if value, ok := hit.Document[column]; ok {
			data[column] = value
		} else {
			data[column] = nil
		}
	}
	return data, nil
}

// ExecuteQueryToRecordsetReader materializes the bounded result and exposes it
// as a DALgo recordset. Projected queries keep exactly the declared columns in
// declaration order; an unprojected query uses the sorted union of returned
// native fields. Every column is Column[any], so boolean, string, integer,
// unsigned integer, float, null, object and array values keep their native
// classes.
func (d *backend) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, options ...recordset.Option) (dal.RecordsetReader, error) {
	if ctx == nil {
		return nil, errors.New("typesense: context is required")
	}
	p, err := d.makePlan(query)
	if err != nil {
		return nil, err
	}
	reader, err := d.newRecordsReader(ctx, p)
	if err != nil {
		return nil, err
	}
	records, err := dal.ReadAllToRecords(ctx, reader)
	if err != nil {
		return nil, err
	}
	columns := recordsetColumns(records, p)
	definitions := make([]recordset.Column[any], len(columns))
	for i, name := range columns {
		definitions[i] = &nativeValueColumn{Column: recordset.NewTypedColumn[any](name, nil)}
	}
	name := p.collection
	if configured := recordset.NewOptions(options...).Name(); configured != "" {
		name = configured
	}
	result := recordset.NewColumnarRecordset(name, definitions...)
	for _, rec := range records {
		rowData, ok := rec.Data().(map[string]any)
		if !ok {
			return nil, malformed("record data is not a native document object")
		}
		row := result.NewRow()
		for _, column := range columns {
			if err = row.SetValueByName(column, rowData[column], result); err != nil {
				return nil, err
			}
		}
	}
	return &recordsetReader{ctx: ctx, recordset: result}, nil
}

func recordsetColumns(records []record.Record, p searchPlan) []string {
	if p.hasProjection {
		return append([]string(nil), p.columns...)
	}
	seen := map[string]bool{}
	for _, rec := range records {
		data, ok := rec.Data().(map[string]any)
		if !ok {
			continue
		}
		for name := range data {
			seen[name] = true
		}
	}
	columns := make([]string, 0, len(seen))
	for name := range seen {
		columns = append(columns, name)
	}
	sort.Strings(columns)
	return columns
}

type nativeValueColumn struct {
	recordset.Column[any]
}

// ValueType is any: one column holds values of every native JSON class in
// different rows, so no single concrete Go type describes it. Keeping the
// explicit interface type prevents a consumer from aligning values by a
// declared affinity the adapter never applied.
func (*nativeValueColumn) ValueType() reflect.Type {
	return reflect.TypeOf((*any)(nil)).Elem()
}

type recordsetReader struct {
	ctx       context.Context
	recordset recordset.Recordset
	position  int
	closed    bool
}

var _ dal.RecordsetReader = (*recordsetReader)(nil)

func (r *recordsetReader) Recordset() recordset.Recordset { return r.recordset }

func (r *recordsetReader) Next() (recordset.Row, recordset.Recordset, error) {
	if r.closed {
		return nil, r.recordset, dal.ErrReaderClosed
	}
	if r.ctx == nil {
		return nil, r.recordset, errors.New("typesense: context is required")
	}
	if err := r.ctx.Err(); err != nil {
		return nil, r.recordset, safeContextError(r.ctx, err)
	}
	if r.position >= r.recordset.RowsCount() {
		return nil, r.recordset, dal.ErrNoMoreRecords
	}
	row := r.recordset.GetRow(r.position)
	r.position++
	return row, r.recordset, nil
}

func (r *recordsetReader) Cursor() (string, error) {
	if r.closed {
		return "", dal.ErrReaderClosed
	}
	return "", notSupported("typesense readers do not issue cursors")
}

func (r *recordsetReader) Close() error {
	r.closed = true
	return nil
}
