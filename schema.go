package dalgo2typesense

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/record"
)

// The adapter implements dbschema.SchemaReader on the backend, so consumers
// reach it with dal.As[dbschema.SchemaReader](db). It reports real Typesense
// collection schemas. It never infers a declared field schema from a sampled
// document.
var _ dbschema.SchemaReader = (*backend)(nil)

type collectionWire struct {
	Name                json.RawMessage `json:"name"`
	Fields              json.RawMessage `json:"fields"`
	DefaultSortingField json.RawMessage `json:"default_sorting_field"`
	NumDocuments        json.RawMessage `json:"num_documents"`
}

type fieldWire struct {
	Name     json.RawMessage `json:"name"`
	Type     json.RawMessage `json:"type"`
	Facet    json.RawMessage `json:"facet"`
	Optional json.RawMessage `json:"optional"`
	Index    json.RawMessage `json:"index"`
	Sort     json.RawMessage `json:"sort"`
	Infix    json.RawMessage `json:"infix"`
}

// MetadataOrigin reports where collection metadata came from. It is
// adapter-local so it does not extend DALgo core. "server" means the server's
// collection catalog answered; "configured" means the adapter additionally
// restricted the result to Config.Collection.
func (d *backend) MetadataOrigin() string {
	if d.cfg.Collection != "" {
		return "configured"
	}
	return "server"
}

// ListCollections returns the collections visible to the configured
// credential. With Config.Collection set, the result is restricted to that
// configured allowlist and the server catalog must actually contain it. A
// forbidden or failed metadata request is returned as an error and is never
// presented as an empty list.
func (d *backend) ListCollections(ctx context.Context, parent *record.Key) ([]dal.CollectionRef, error) {
	if parent != nil {
		return nil, schemaNotSupported("ListCollections", "Typesense has no hierarchical collection catalog")
	}
	op, err := d.newOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer op.close()
	resp, err := d.request(op, "/collections", nil)
	if err != nil {
		return nil, metadataError(err)
	}
	entries, err := decodeCollectionList(resp.body)
	if err != nil {
		return nil, err
	}
	if d.cfg.Collection != "" {
		for _, entry := range entries {
			if entry.Name == d.cfg.Collection {
				return []dal.CollectionRef{dal.NewRootCollectionRef(entry.Name, "")}, nil
			}
		}
		return nil, errors.New("typesense: configured collection is absent from metadata")
	}
	collections := make([]dal.CollectionRef, 0, len(entries))
	for _, entry := range entries {
		collections = append(collections, dal.NewRootCollectionRef(entry.Name, ""))
	}
	return collections, nil
}

// DescribeCollection reports the real Typesense schema for one collection.
// Typesense has an implicit string id primary key; it is reported as the
// primary key even when the id field is not declared in the schema.
func (d *backend) DescribeCollection(ctx context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	if err := d.validateMetadataRef(ref); err != nil {
		return nil, err
	}
	op, err := d.newOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer op.close()
	resp, err := d.request(op, collectionPath(ref.Name()), nil)
	if err != nil {
		return nil, metadataError(err)
	}
	entry, err := decodeCollection(resp.body)
	if err != nil {
		return nil, err
	}
	if entry.Name != ref.Name() {
		return nil, malformed("collection metadata name is mismatched")
	}
	return entry.definition(), nil
}

// ListIndexes reports no SQL-style named secondary indexes: Typesense indexes
// fields according to the collection schema and does not expose a separate
// index catalog. The empty result is honest and is not a metadata failure.
func (d *backend) ListIndexes(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	if err := d.validateMetadataRef(ref); err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	return []dbschema.IndexDef{}, nil
}

// ListConstraints is unsupported: Typesense has no SQL-style constraint
// catalog.
func (d *backend) ListConstraints(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	if err := d.validateMetadataRef(ref); err != nil {
		return nil, err
	}
	return nil, schemaNotSupported("ListConstraints", "Typesense has no declared SQL constraints")
}

// ListReferrers is unsupported: Typesense has no foreign-key catalog.
func (d *backend) ListReferrers(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.Referrer, error) {
	if err := d.validateMetadataRef(ref); err != nil {
		return nil, err
	}
	return nil, schemaNotSupported("ListReferrers", "Typesense has no declared foreign keys")
}

func (d *backend) validateMetadataRef(ref *dal.CollectionRef) error {
	if ref == nil {
		return schemaNotSupported("DescribeCollection", "collection reference is required")
	}
	if ref.Parent() != nil || ref.Schema() != "" || ref.Database() != "" || ref.Alias() != "" ||
		ref.ScanLimit() != 0 || len(ref.ScanOrders()) != 0 {
		return schemaNotSupported("DescribeCollection", "only one unqualified root collection is described")
	}
	if d.cfg.Collection != "" && ref.Name() != d.cfg.Collection {
		return schemaNotSupported("DescribeCollection", "only the configured collection is described")
	}
	if !reCollectionName.MatchString(ref.Name()) {
		return schemaNotSupported("DescribeCollection", "collection name is not supported")
	}
	return nil
}

func decodeCollectionList(payload []byte) ([]collectionEntry, error) {
	var raw json.RawMessage
	if err := decodeJSON(payload, &raw); err != nil {
		return nil, err
	}
	entries, err := requiredArray(raw, "collections")
	if err != nil {
		return nil, err
	}
	result := make([]collectionEntry, 0, len(entries))
	for _, entry := range entries {
		decoded, err := decodeCollection(entry)
		if err != nil {
			return nil, err
		}
		result = append(result, decoded)
	}
	return result, nil
}

type collectionEntry struct {
	Name                string
	Fields              []fieldEntry
	DefaultSortingField string
	NumDocuments        int
}

type fieldEntry struct {
	Name     string
	Type     string
	Facet    bool
	Optional bool
	Index    bool
	Sort     bool
	Infix    bool
}

func decodeCollection(raw json.RawMessage) (collectionEntry, error) {
	item, err := requiredObject(raw, "collection")
	if err != nil {
		return collectionEntry{}, err
	}
	wire := collectionWire{
		Name:                item["name"],
		Fields:              item["fields"],
		DefaultSortingField: item["default_sorting_field"],
		NumDocuments:        item["num_documents"],
	}
	entry := collectionEntry{}
	if entry.Name, err = requiredString(wire.Name, "collection name"); err != nil {
		return collectionEntry{}, err
	}
	if !reCollectionName.MatchString(entry.Name) {
		return collectionEntry{}, malformed("collection name is not supported")
	}
	if wire.DefaultSortingField != nil {
		if entry.DefaultSortingField, err = requiredString(wire.DefaultSortingField, "default_sorting_field"); err != nil {
			return collectionEntry{}, err
		}
	}
	if wire.NumDocuments != nil {
		if entry.NumDocuments, _, err = optionalInt(wire.NumDocuments, "num_documents"); err != nil {
			return collectionEntry{}, err
		}
	}
	rawFields, err := requiredArray(wire.Fields, "collection fields")
	if err != nil {
		return collectionEntry{}, err
	}
	seen := map[string]bool{}
	for _, rawField := range rawFields {
		fieldItem, err := requiredObject(rawField, "collection field")
		if err != nil {
			return collectionEntry{}, err
		}
		fieldWireValue := fieldWire{
			Name:     fieldItem["name"],
			Type:     fieldItem["type"],
			Facet:    fieldItem["facet"],
			Optional: fieldItem["optional"],
			Index:    fieldItem["index"],
			Sort:     fieldItem["sort"],
			Infix:    fieldItem["infix"],
		}
		field := fieldEntry{Index: true}
		if field.Name, err = requiredString(fieldWireValue.Name, "field name"); err != nil {
			return collectionEntry{}, err
		}
		if !validateSchemaFieldName(field.Name) {
			return collectionEntry{}, malformed("field name is not supported")
		}
		if seen[field.Name] {
			return collectionEntry{}, malformed("duplicate field name")
		}
		seen[field.Name] = true
		if field.Type, err = requiredString(fieldWireValue.Type, "field type"); err != nil {
			return collectionEntry{}, err
		}
		if !supportedSchemaType(field.Type) {
			return collectionEntry{}, malformed("field type is not supported")
		}
		if field.Facet, _, err = optionalBool(fieldWireValue.Facet, "field facet"); err != nil {
			return collectionEntry{}, err
		}
		if field.Optional, _, err = optionalBool(fieldWireValue.Optional, "field optional"); err != nil {
			return collectionEntry{}, err
		}
		if field.Index, _, err = optionalBool(fieldWireValue.Index, "field index"); err != nil {
			return collectionEntry{}, err
		}
		if field.Sort, _, err = optionalBool(fieldWireValue.Sort, "field sort"); err != nil {
			return collectionEntry{}, err
		}
		if field.Infix, _, err = optionalBool(fieldWireValue.Infix, "field infix"); err != nil {
			return collectionEntry{}, err
		}
		entry.Fields = append(entry.Fields, field)
	}
	return entry, nil
}

func (e collectionEntry) definition() *dbschema.CollectionDef {
	fields := make([]dbschema.FieldDef, 0, len(e.Fields))
	columns := make([]dbschema.SourceColumnDef, 0, len(e.Fields))
	for _, field := range e.Fields {
		fields = append(fields, dbschema.FieldDef{
			Name:     dal.FieldName(field.Name),
			Type:     portableType(field.Type),
			Nullable: field.Optional,
		})
		columns = append(columns, dbschema.SourceColumnDef{
			Name:         field.Name,
			DeclaredType: field.Type,
			NotNull:      !field.Optional,
		})
	}
	return &dbschema.CollectionDef{
		Name:       e.Name,
		Fields:     fields,
		PrimaryKey: []dal.FieldName{"id"},
		SourceDefinition: &dbschema.SourceDefinition{
			Dialect: "typesense",
			Columns: columns,
		},
	}
}

func portableType(native string) dbschema.Type {
	switch native {
	case "string", "string[]":
		return dbschema.String
	case "int32", "int64", "int32[]", "int64[]":
		return dbschema.Int
	case "float", "float[]":
		return dbschema.Float
	case "bool", "bool[]":
		return dbschema.Bool
	default:
		// object, object[], geopoint and geopolygon have no portable
		// counterpart. Null means "no portable type" and the exact native type
		// remains in SourceDefinition.DeclaredType.
		return dbschema.Null
	}
}

func validateSchemaFieldName(name string) bool {
	if name == "" || strings.HasPrefix(name, "$") || strings.ContainsAny(name, ",\t\r\n\x00") {
		return false
	}
	return true
}

func supportedSchemaType(native string) bool {
	switch native {
	case "string", "string[]", "int32", "int64", "int32[]", "int64[]",
		"float", "float[]", "bool", "bool[]", "object", "object[]",
		"geopoint", "geopoint[]", "geopolygon":
		return true
	default:
		return false
	}
}

func metadataError(err error) error {
	if errors.Is(err, ErrPermissionDenied) {
		return errors.Join(ErrMetadataForbidden, err)
	}
	return err
}

func schemaNotSupported(op, reason string) error {
	return &dbschema.NotSupportedError{Op: op, Backend: adapterName, Reason: reason}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrCanceled
	}
	if err := ctx.Err(); err != nil {
		return safeContextError(ctx, err)
	}
	return nil
}
