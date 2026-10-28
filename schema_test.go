package dalgo2typesense

import (
	"errors"
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// supportedArrayTypes is every array class the metadata decoder accepts. None
// has a portable dbschema counterpart because dbschema.Type is scalar-only.
var supportedArrayTypes = []string{
	"string[]",
	"int32[]",
	"int64[]",
	"float[]",
	"bool[]",
	"object[]",
	"geopoint[]",
}

func TestPortableType_ScalarsKeepTheirPortableCounterparts(t *testing.T) {
	cases := []struct {
		native string
		want   dbschema.Type
	}{
		{"string", dbschema.String},
		{"int32", dbschema.Int},
		{"int64", dbschema.Int},
		{"float", dbschema.Float},
		{"bool", dbschema.Bool},
	}
	for _, tc := range cases {
		t.Run(tc.native, func(t *testing.T) {
			if got := portableType(tc.native); got != tc.want {
				t.Fatalf("portableType(%q) = %v, want %v", tc.native, got, tc.want)
			}
		})
	}
}

func TestPortableType_EverySupportedArrayClassHasNoPortableCounterpart(t *testing.T) {
	for _, native := range supportedArrayTypes {
		t.Run(native, func(t *testing.T) {
			if !supportedSchemaType(native) {
				t.Fatalf("%q is not an accepted native array class", native)
			}
			if got := portableType(native); got != dbschema.Null {
				t.Fatalf("portableType(%q) = %v, want dbschema.Null: an array must never become a scalar portable type", native, got)
			}
		})
	}
}

func TestDefinition_ArraysKeepDeclaredTypeWithoutFalsePortableScalar(t *testing.T) {
	names := []string{
		"strings",
		"int32s",
		"int64s",
		"floats",
		"bools",
		"objects",
		"geopoints",
	}
	entry := collectionEntry{Name: "arrays"}
	for i, native := range supportedArrayTypes {
		entry.Fields = append(entry.Fields, fieldEntry{Name: names[i], Type: native})
	}

	def := entry.definition()
	if def.SourceDefinition == nil {
		t.Fatal("source definition is missing")
	}
	if len(def.Fields) != len(names)+1 || len(def.SourceDefinition.Columns) != len(names)+1 {
		t.Fatalf("fields/columns = %d/%d, want %d including the implicit id",
			len(def.Fields), len(def.SourceDefinition.Columns), len(names)+1)
	}
	for i, native := range supportedArrayTypes {
		field := fieldDefByName(t, def.Fields, names[i])
		if field.Type != dbschema.Null {
			t.Fatalf("%q portable type = %v, want dbschema.Null (never a scalar)", native, field.Type)
		}
		column := sourceColumnByName(t, def.SourceDefinition.Columns, names[i])
		if column.DeclaredType != native {
			t.Fatalf("%q declared type = %q, want %q", names[i], column.DeclaredType, native)
		}
	}
}

func TestDefinition_AddsImplicitStringIDExactlyOnce(t *testing.T) {
	entry := collectionEntry{Name: "books", Fields: []fieldEntry{
		{Name: "title", Type: "string"},
		{Name: "rank", Type: "int64"},
	}}

	def := entry.definition()
	if !reflect.DeepEqual(def.PrimaryKey, []dal.FieldName{"id"}) {
		t.Fatalf("primary key = %v, want [id]", def.PrimaryKey)
	}
	if countFieldDefs(def.Fields, "id") != 1 {
		t.Fatalf("portable id fields = %d, want exactly 1", countFieldDefs(def.Fields, "id"))
	}
	if countSourceColumns(def.SourceDefinition.Columns, "id") != 1 {
		t.Fatalf("source id columns = %d, want exactly 1", countSourceColumns(def.SourceDefinition.Columns, "id"))
	}
	idField := fieldDefByName(t, def.Fields, "id")
	if idField.Type != dbschema.String || idField.Nullable {
		t.Fatalf("implicit id field = %+v, want non-nullable string", idField)
	}
	idColumn := sourceColumnByName(t, def.SourceDefinition.Columns, "id")
	if idColumn.DeclaredType != "string" || !idColumn.NotNull {
		t.Fatalf("implicit id column = %+v, want declared string and NOT NULL", idColumn)
	}
	if len(def.Fields) != len(entry.Fields)+1 {
		t.Fatalf("fields = %d, want %d including implicit id", len(def.Fields), len(entry.Fields)+1)
	}
}

func TestDefinition_DeclaredStringIDIsNotDuplicated(t *testing.T) {
	entry := collectionEntry{Name: "books", Fields: []fieldEntry{
		{Name: "id", Type: "string"},
		{Name: "title", Type: "string"},
	}}

	def := entry.definition()
	if !reflect.DeepEqual(def.PrimaryKey, []dal.FieldName{"id"}) {
		t.Fatalf("primary key = %v, want [id]", def.PrimaryKey)
	}
	if countFieldDefs(def.Fields, "id") != 1 {
		t.Fatalf("portable id fields = %d, want exactly 1", countFieldDefs(def.Fields, "id"))
	}
	if countSourceColumns(def.SourceDefinition.Columns, "id") != 1 {
		t.Fatalf("source id columns = %d, want exactly 1", countSourceColumns(def.SourceDefinition.Columns, "id"))
	}
	if def.Fields[0].Name != "id" || def.SourceDefinition.Columns[0].Name != "id" {
		t.Fatalf("declared id order was not preserved: %+v / %+v", def.Fields[0], def.SourceDefinition.Columns[0])
	}
	idField := fieldDefByName(t, def.Fields, "id")
	if idField.Type != dbschema.String || idField.Nullable {
		t.Fatalf("declared id field = %+v, want non-nullable string", idField)
	}
	idColumn := sourceColumnByName(t, def.SourceDefinition.Columns, "id")
	if idColumn.DeclaredType != "string" || !idColumn.NotNull {
		t.Fatalf("declared id column = %+v, want declared string and NOT NULL", idColumn)
	}
	if len(def.Fields) != len(entry.Fields) {
		t.Fatalf("fields = %d, want %d without an extra id", len(def.Fields), len(entry.Fields))
	}
}

func TestDecodeCollection_RejectsExplicitNonStringID(t *testing.T) {
	raw := []byte(`{"name":"books","fields":[{"name":"id","type":"int64"},{"name":"title","type":"string"}]}`)
	if _, err := decodeCollection(raw); err == nil || !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("decodeCollection err = %v, want ErrMalformedResponse for a non-string explicit id", err)
	}
}

func TestDecodeCollection_AcceptsDeclaredStringIDWithoutInventingAnother(t *testing.T) {
	raw := []byte(`{"name":"books","fields":[{"name":"id","type":"string"},{"name":"title","type":"string"}]}`)
	entry, err := decodeCollection(raw)
	if err != nil {
		t.Fatalf("decodeCollection: %v", err)
	}
	def := entry.definition()
	if countFieldDefs(def.Fields, "id") != 1 || countSourceColumns(def.SourceDefinition.Columns, "id") != 1 {
		t.Fatalf("explicit string id was duplicated: %+v / %+v", def.Fields, def.SourceDefinition.Columns)
	}
}

func fieldDefByName(t *testing.T, fields []dbschema.FieldDef, name string) dbschema.FieldDef {
	t.Helper()
	found := -1
	for i, field := range fields {
		if string(field.Name) == name {
			if found >= 0 {
				t.Fatalf("field %q appears more than once", name)
			}
			found = i
		}
	}
	if found < 0 {
		t.Fatalf("field %q is missing", name)
	}
	return fields[found]
}

func sourceColumnByName(t *testing.T, columns []dbschema.SourceColumnDef, name string) dbschema.SourceColumnDef {
	t.Helper()
	found := -1
	for i, column := range columns {
		if column.Name == name {
			if found >= 0 {
				t.Fatalf("source column %q appears more than once", name)
			}
			found = i
		}
	}
	if found < 0 {
		t.Fatalf("source column %q is missing", name)
	}
	return columns[found]
}

func countFieldDefs(fields []dbschema.FieldDef, name string) int {
	count := 0
	for _, field := range fields {
		if string(field.Name) == name {
			count++
		}
	}
	return count
}

func countSourceColumns(columns []dbschema.SourceColumnDef, name string) int {
	count := 0
	for _, column := range columns {
		if column.Name == name {
			count++
		}
	}
	return count
}
