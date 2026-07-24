package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/template"
	"unicode"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/imports"
)

// RelationType identifies the cardinality of a detected model relationship.
type RelationType string

const (
	// RelHasMany marks a one-to-many relationship, backed by a slice field.
	RelHasMany RelationType = "HasMany"

	// RelBelongsTo marks a many-to-one (or one-to-one) relationship, backed
	// by a pointer or bare struct field.
	RelBelongsTo RelationType = "BelongsTo"
)

// Permission describes the read/write visibility of a field, derived from
// GORM's "->" and "<-" tag directives. It controls whether a field
// participates in generated SELECT, INSERT, and UPDATE statements.
type Permission int

const (
	// PermReadWrite is the default: the field is selected, inserted, and updated.
	PermReadWrite Permission = iota
	// PermReadOnly corresponds to gorm:"->"; the field is only ever selected,
	// never written by generated Insert/Update statements.
	PermReadOnly
	// PermWriteOnly corresponds to gorm:"<-"; the field is only ever written,
	// never selected by generated Get statements.
	PermWriteOnly
	// PermCreateOnly corresponds to gorm:"<-:create"; the field is inserted
	// but never updated.
	PermCreateOnly
	// PermUpdateOnly corresponds to gorm:"<-:update"; the field is updated
	// but never inserted.
	PermUpdateOnly
)

// Relation describes a HasMany or BelongsTo association discovered on a
// model, used to execute association preloading when requested.
type Relation struct {
	FieldName   string // Struct field name on the parent, e.g. "User" or "Orders".
	TargetModel string // Related model type name, e.g. "User" or "Order".
	Type        RelationType
	ForeignKey  string // Column name storing the reference ID.
	References  string // Column name being referenced.
	IsPointer   bool   // True if the field is a pointer (*User vs User).
}

// Field describes a single scalar struct field mapped to a database column,
// including its nullability and read/write permission derived from GORM tags.
type Field struct {
	Name       string // Go struct field name, e.g. "Email".
	Type       string // Go type as written in source, e.g. "string" or "*string".
	Column     string // Database column name, e.g. "email".
	IsPK       bool   // True if this field is the primary key.
	IsIgnore   bool   // True if the field is excluded entirely (gorm:"-").
	Nullable   bool   // True if the column may return SQL NULL (gorm:"null" or a pointer type).
	Permission Permission
}

// Writable reports whether this field should appear in an INSERT statement.
func (f Field) Writable() bool {
	return !f.IsPK && f.Permission != PermReadOnly && f.Permission != PermUpdateOnly
}

// Updatable reports whether this field should appear in an UPDATE statement.
func (f Field) Updatable() bool {
	return !f.IsPK && f.Permission != PermReadOnly && f.Permission != PermCreateOnly
}

// Selectable reports whether this field should appear in a SELECT statement.
func (f Field) Selectable() bool {
	return f.Permission != PermWriteOnly
}

// BaseType strips one leading pointer indirection, so callers can tell a
// *string field apart from its underlying string type for scan-target
// construction.
func (f Field) BaseType() string {
	return strings.TrimPrefix(f.Type, "*")
}

// Model describes a parsed Go struct and everything needed to render its
// generated CRUD query file: table name, columns, primary key, and any
// HasMany/BelongsTo relations to other known models.
type Model struct {
	Package        string // Generated package name, e.g. "queries".
	ModelPkg       string // Full import path of the source models package.
	ModelPkgAlias  string // Import alias for the source models package, e.g. "models".
	Name           string // Go type name, e.g. "User".
	Table          string // Database table name, e.g. "users".
	Fields         []Field
	PK             *Field
	Relations      []Relation
	AllKnownModels map[string]Model // All models in the parsed package, keyed by type name.
}

// FieldByColumn finds a struct Field by its database column name.
func (m Model) FieldByColumn(col string) *Field {
	for i := range m.Fields {
		if m.Fields[i].Column == col {
			return &m.Fields[i]
		}
	}
	return nil
}

// --- Template method helpers ---

func (m Model) SelectableFields() []Field {
	fields := make([]Field, 0, len(m.Fields))
	for _, f := range m.Fields {
		if f.Selectable() {
			fields = append(fields, f)
		}
	}
	return fields
}

func (m Model) WritableFields() []Field {
	fields := make([]Field, 0, len(m.Fields))
	for _, f := range m.Fields {
		if f.Writable() {
			fields = append(fields, f)
		}
	}
	return fields
}

func (m Model) UpdatableFields() []Field {
	fields := make([]Field, 0, len(m.Fields))
	for _, f := range m.Fields {
		if f.Updatable() {
			fields = append(fields, f)
		}
	}
	return fields
}

func (m Model) AllColumns(prefix string) string {
	fields := m.SelectableFields()
	cols := make([]string, len(fields))
	for i, f := range fields {
		if prefix != "" {
			cols[i] = fmt.Sprintf("%s.%s", prefix, f.Column)
		} else {
			cols[i] = f.Column
		}
	}
	return strings.Join(cols, ", ")
}

func (m Model) InsertColumns() string {
	fields := m.WritableFields()
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = f.Column
	}
	return strings.Join(cols, ", ")
}

func (m Model) InsertPlaceholders() string {
	fields := m.WritableFields()
	placeholders := make([]string, len(fields))
	for i := range fields {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	return strings.Join(placeholders, ", ")
}

func (m Model) UpdateSetClause() string {
	fields := m.UpdatableFields()
	clauses := make([]string, len(fields))
	for i, f := range fields {
		clauses[i] = fmt.Sprintf("%s = $%d", f.Column, i+1)
	}
	return strings.Join(clauses, ", ")
}

func (m Model) UpdatePKPlaceholderIdx() int {
	return len(m.UpdatableFields()) + 1
}

func scanTarget(varName string, f Field) string {
	if f.Nullable && !strings.HasPrefix(f.Type, "*") {
		return fmt.Sprintf("scanNullable(&%s.%s)", varName, f.Name)
	}
	return fmt.Sprintf("&%s.%s", varName, f.Name)
}

func (m Model) AllScanArgs(varName string) string {
	fields := m.SelectableFields()
	args := make([]string, len(fields))
	for i, f := range fields {
		args[i] = scanTarget(varName, f)
	}
	return strings.Join(args, ", ")
}

func (m Model) WritableScanArgs(varName string) string {
	fields := m.WritableFields()
	args := make([]string, len(fields))
	for i, f := range fields {
		args[i] = fmt.Sprintf("&%s.%s", varName, f.Name)
	}
	return strings.Join(args, ", ")
}

func (m Model) UpdatableScanArgs(varName string) string {
	fields := m.UpdatableFields()
	args := make([]string, len(fields))
	for i, f := range fields {
		args[i] = fmt.Sprintf("&%s.%s", varName, f.Name)
	}
	return strings.Join(args, ", ")
}

func main() {
	inputPkg := flag.String("input", "./example/models", "Path to package containing models")
	outDir := flag.String("out", "./example/queries", "Destination directory for generated code")
	outPkg := flag.String("pkg", "queries", "Package name for generated code")
	flag.Parse()

	parsedModels, fullPkgPath, err := parsePackage(*inputPkg)
	if err != nil {
		log.Fatalf("query-gen: parsing package %q: %v", *inputPkg, err)
	}
	if len(parsedModels) == 0 {
		log.Fatalf("query-gen: no exported structs found in %q", *inputPkg)
	}

	modelMap := make(map[string]Model, len(parsedModels))
	for _, m := range parsedModels {
		modelMap[m.Name] = m
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("query-gen: creating output directory %q: %v", *outDir, err)
	}

	if err := generateRuntimeFile(*outDir, *outPkg); err != nil {
		log.Fatalf("query-gen: generating runtime file: %v", err)
	}

	pkgAlias := filepath.Base(fullPkgPath)

	for _, model := range parsedModels {
		if model.PK == nil {
			log.Printf("query-gen: skipping %q: no primary key field found", model.Name)
			continue
		}

		model.Package = *outPkg
		model.ModelPkg = fullPkgPath
		model.ModelPkgAlias = pkgAlias
		model.AllKnownModels = modelMap

		code, err := generateQueries(model)
		if err != nil {
			log.Fatalf("query-gen: generating code for %q: %v", model.Name, err)
		}

		fileName := fmt.Sprintf("%s_gen.go", toSnakeCase(model.Name))
		filePath := filepath.Join(*outDir, fileName)

		formatted, err := imports.Process(filePath, []byte(code), nil)
		if err != nil {
			log.Printf("query-gen: warning: goimports failed for %q: %v (writing unformatted source)", model.Name, err)
			formatted = []byte(code)
		}

		if err := os.WriteFile(filePath, formatted, 0o644); err != nil {
			log.Fatalf("query-gen: writing %q: %v", filePath, err)
		}
	}
}

func parsePackage(pattern string) ([]Model, string, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo,
	}

	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return nil, "", fmt.Errorf("loading package %q: %w", pattern, err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		return nil, "", fmt.Errorf("package %q has compile errors", pattern)
	}
	if len(pkgs) == 0 {
		return nil, "", fmt.Errorf("no packages matched pattern %q", pattern)
	}

	var result []Model
	fullPkgPath := pkgs[0].PkgPath

	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				typeSpec, ok := n.(*ast.TypeSpec)
				if !ok || !typeSpec.Name.IsExported() {
					return true
				}

				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					return true
				}

				model := Model{
					Name:  typeSpec.Name.Name,
					Table: toSnakeCase(typeSpec.Name.Name) + "s",
				}

				for _, field := range structType.Fields.List {
					if len(field.Names) == 0 || !field.Names[0].IsExported() {
						continue
					}

					fieldName := field.Names[0].Name
					rawTag := ""
					if field.Tag != nil {
						rawTag = strings.Trim(field.Tag.Value, "`")
					}

					if rel, ok := detectRelation(field, rawTag, model.Name); ok {
						model.Relations = append(model.Relations, rel)
						continue
					}

					fieldType := types.ExprString(field.Type)
					parsedField := parseFieldTags(fieldName, fieldType, rawTag)

					if parsedField.IsIgnore {
						continue
					}

					model.Fields = append(model.Fields, parsedField)
					if parsedField.IsPK {
						model.PK = &model.Fields[len(model.Fields)-1]
					}
				}

				if model.PK == nil {
					for i := range model.Fields {
						if strings.EqualFold(model.Fields[i].Name, "ID") {
							model.Fields[i].IsPK = true
							model.PK = &model.Fields[i]
							break
						}
					}
				}

				result = append(result, model)
				return true
			})
		}
	}

	return result, fullPkgPath, nil
}

func detectRelation(field *ast.Field, rawTag string, parentModelName string) (Relation, bool) {
	fieldName := field.Names[0].Name
	rel := Relation{FieldName: fieldName}

	structTag := reflect.StructTag(rawTag)
	gormTag := structTag.Get("gorm")

	switch t := field.Type.(type) {
	case *ast.ArrayType:
		elemIdent, ok := t.Elt.(*ast.Ident)
		if !ok || !elemIdent.IsExported() {
			return rel, false
		}
		rel.Type = RelHasMany
		rel.TargetModel = elemIdent.Name
	case *ast.StarExpr:
		ident, ok := t.X.(*ast.Ident)
		if !ok || !ident.IsExported() {
			return rel, false
		}
		rel.Type = RelBelongsTo
		rel.TargetModel = ident.Name
		rel.IsPointer = true
	case *ast.Ident:
		if !t.IsExported() {
			return rel, false
		}
		rel.Type = RelBelongsTo
		rel.TargetModel = t.Name
		rel.IsPointer = false
	default:
		return rel, false
	}

	if gormTag != "" {
		for part := range strings.SplitSeq(gormTag, ";") {
			part = strings.TrimSpace(part)
			switch {
			case strings.HasPrefix(part, "foreignKey:"):
				rel.ForeignKey = toSnakeCase(strings.TrimPrefix(part, "foreignKey:"))
			case strings.HasPrefix(part, "references:"):
				rel.References = toSnakeCase(strings.TrimPrefix(part, "references:"))
			}
		}
	}

	switch rel.Type {
	case RelHasMany:
		if rel.ForeignKey == "" {
			rel.ForeignKey = toSnakeCase(parentModelName) + "_id"
		}
	case RelBelongsTo:
		if rel.ForeignKey == "" {
			rel.ForeignKey = toSnakeCase(rel.FieldName) + "_id"
		}
	}

	return rel, true
}

func parseFieldTags(fieldName, fieldType, rawTag string) Field {
	f := Field{
		Name:       fieldName,
		Type:       fieldType,
		Column:     toSnakeCase(fieldName),
		IsPK:       strings.EqualFold(fieldName, "ID"),
		Permission: PermReadWrite,
	}

	f.Nullable = strings.HasPrefix(fieldType, "*")

	if rawTag == "" {
		return f
	}

	structTag := reflect.StructTag(rawTag)
	gormTag := structTag.Get("gorm")
	if gormTag == "" {
		return f
	}

	notNullSeen := false

	for part := range strings.SplitSeq(gormTag, ";") {
		part = strings.TrimSpace(part)
		lower := strings.ToLower(part)

		switch {
		case part == "-":
			f.IsIgnore = true
			return f

		case lower == "primarykey" || lower == "primary_key":
			f.IsPK = true

		case strings.HasPrefix(lower, "column:"):
			f.Column = part[len("column:"):]

		case lower == "not null" || lower == "notnull":
			notNullSeen = true
			f.Nullable = false

		case lower == "null":
			f.Nullable = true

		case part == "->":
			f.Permission = PermReadOnly

		case part == "<-":
			f.Permission = PermWriteOnly

		case lower == "<-:create":
			f.Permission = PermCreateOnly

		case lower == "<-:update":
			f.Permission = PermUpdateOnly
		}
	}

	if f.IsPK {
		f.Nullable = false
	} else if !notNullSeen && !f.Nullable {
		f.Nullable = false
	}

	return f
}

// --- Runtime Helper Template ---

const runtimeTemplate = `// Code generated by query-gen. DO NOT EDIT.
package {{.Package}}

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"time"
)

type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// QueryOptions provides optional filtering, ordering, grouping, pagination, and association preloading for queries.
type QueryOptions struct {
	Where               string
	Args                []any
	OrderBy             string
	GroupBy             string
	Limit               int
	Offset              int
	PreloadAssociations bool
}

type QueryOption func(*QueryOptions)

func WithWhere(where string, args ...any) QueryOption {
	return func(o *QueryOptions) {
		o.Where = where
		o.Args = append(o.Args, args...)
	}
}

func WithOrderBy(orderBy string) QueryOption {
	return func(o *QueryOptions) {
		o.OrderBy = orderBy
	}
}

func WithGroupBy(groupBy string) QueryOption {
	return func(o *QueryOptions) {
		o.GroupBy = groupBy
	}
}

func WithLimit(limit int) QueryOption {
	return func(o *QueryOptions) {
		o.Limit = limit
	}
}

func WithOffset(offset int) QueryOption {
	return func(o *QueryOptions) {
		o.Offset = offset
	}
}

// WithPreloadAssociations enables or disables preloading associated model relations.
func WithPreloadAssociations(preload bool) QueryOption {
	return func(o *QueryOptions) {
		o.PreloadAssociations = preload
	}
}

func parseQueryOptions(opts ...QueryOption) QueryOptions {
	var cfg QueryOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

func applyQueryOptions(defaultPK string, opts ...QueryOption) (string, []any, QueryOptions) {
	cfg := parseQueryOptions(opts...)

	var sb strings.Builder
	args := cfg.Args

	if cfg.Where != "" {
		sb.WriteString(" WHERE ")
		sb.WriteString(cfg.Where)
	}

	if cfg.GroupBy != "" {
		sb.WriteString(" GROUP BY ")
		sb.WriteString(cfg.GroupBy)
	}

	if cfg.OrderBy != "" {
		sb.WriteString(" ORDER BY ")
		sb.WriteString(cfg.OrderBy)
	} else if defaultPK != "" {
		sb.WriteString(" ORDER BY ")
		sb.WriteString(defaultPK)
		sb.WriteString(" ASC")
	}

	if cfg.Limit > 0 {
		args = append(args, cfg.Limit)
		fmt.Fprintf(&sb, " LIMIT $%d", len(args))
	}

	if cfg.Offset > 0 {
		args = append(args, cfg.Offset)
		fmt.Fprintf(&sb, " OFFSET $%d", len(args))
	}

	return sb.String(), args, cfg
}

func scanNullable[T any](dst *T) *nullableScanner[T] {
	return &nullableScanner[T]{dst: dst}
}

type nullableScanner[T any] struct {
	dst *T
}

func (n *nullableScanner[T]) Scan(src any) error {
	if src == nil {
		var zero T
		*n.dst = zero
		return nil
	}

	if scanner, ok := any(n.dst).(sql.Scanner); ok {
		return scanner.Scan(src)
	}

	if v, ok := src.(T); ok {
		*n.dst = v
		return nil
	}

	return convertAssign(n.dst, src)
}

func convertAssign[T any](dst *T, src any) error {
	if src == nil {
		var zero T
		*dst = zero
		return nil
	}

	if v, ok := src.(T); ok {
		*dst = v
		return nil
	}

	vDst := reflect.ValueOf(dst).Elem()
	vSrc := reflect.ValueOf(src)

	if vSrc.Type().ConvertibleTo(vDst.Type()) {
		vDst.Set(vSrc.Convert(vDst.Type()))
		return nil
	}

	if vDst.Kind() == reflect.String && vSrc.Kind() == reflect.Slice && vSrc.Type().Elem().Kind() == reflect.Uint8 {
		vDst.SetString(string(vSrc.Bytes()))
		return nil
	}
	if vDst.Kind() == reflect.Slice && vDst.Type().Elem().Kind() == reflect.Uint8 && vSrc.Kind() == reflect.String {
		vDst.SetBytes([]byte(vSrc.String()))
		return nil
	}

	if _, ok := any(dst).(*time.Time); ok {
		switch s := src.(type) {
		case string:
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return fmt.Errorf("convertAssign: failed to parse time string %q: %w", s, err)
			}
			vDst.Set(reflect.ValueOf(t))
			return nil
		case []byte:
			t, err := time.Parse(time.RFC3339, string(s))
			if err != nil {
				return fmt.Errorf("convertAssign: failed to parse time bytes %q: %w", s, err)
			}
			vDst.Set(reflect.ValueOf(t))
			return nil
		}
	}

	if isNumeric(vDst.Kind()) && isNumeric(vSrc.Kind()) {
		if vDst.Kind() >= reflect.Int && vDst.Kind() <= reflect.Int64 {
			vDst.SetInt(vSrc.Int())
			return nil
		}
		if vDst.Kind() >= reflect.Uint && vDst.Kind() <= reflect.Uint64 {
			vDst.SetUint(vSrc.Uint())
			return nil
		}
		if vDst.Kind() == reflect.Float32 || vDst.Kind() == reflect.Float64 {
			vDst.SetFloat(vSrc.Float())
			return nil
		}
	}

	return fmt.Errorf("scanNullable: cannot convert %T (%v) to %T", src, src, dst)
}

func isNumeric(k reflect.Kind) bool {
	return (k >= reflect.Int && k <= reflect.Complex128)
}

func isZero[T comparable](v T) bool {
	var zero T
	return v == zero
}
`

func generateRuntimeFile(outDir, outPkg string) error {
	tmpl, err := template.New("runtime").Parse(runtimeTemplate)
	if err != nil {
		return fmt.Errorf("parsing runtime template: %w", err)
	}

	var buf bytes.Buffer
	data := map[string]string{"Package": outPkg}
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("executing runtime template: %w", err)
	}

	filePath := filepath.Join(outDir, "runtime_gen.go")

	formatted, err := imports.Process(filePath, buf.Bytes(), nil)
	if err != nil {
		formatted = buf.Bytes()
	}

	if err := os.WriteFile(filePath, formatted, 0o644); err != nil {
		return fmt.Errorf("writing runtime_gen.go: %w", err)
	}
	return nil
}

// --- Model Code Template ---

const codeTemplate = `// Code generated by query-gen. DO NOT EDIT.
package {{.Package}}

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"{{.ModelPkg}}"
)

// Insert{{.Name}} inserts a new {{.Name}} record into the {{.Table}} table.
func Insert{{.Name}}(ctx context.Context, db DBTX, m *{{.ModelPkgAlias}}.{{.Name}}) error {
	if db == nil {
		return errors.New("insert{{.Name}}: db is nil")
	}
	if m == nil {
		return errors.New("insert{{.Name}}: m is nil")
	}

	const query = ` + "`" + `
		INSERT INTO {{.Table}} ({{.InsertColumns}})
		VALUES ({{.InsertPlaceholders}})
		RETURNING {{.PK.Column}}
	` + "`" + `

	if err := db.QueryRowContext(ctx, query, {{.WritableScanArgs "m"}}).Scan(&m.{{.PK.Name}}); err != nil {
		return fmt.Errorf("insert{{.Name}}: %w", err)
	}
	return nil
}

// Get{{.Name}}ByID retrieves a single {{.Name}} record from {{.Table}} by its primary key.
func Get{{.Name}}ByID(ctx context.Context, db DBTX, id {{.PK.Type}}, opts ...QueryOption) (*{{.ModelPkgAlias}}.{{.Name}}, error) {
	if db == nil {
		return nil, errors.New("get{{.Name}}ByID: db is nil")
	}

	cfg := parseQueryOptions(opts...)

	const query = ` + "`" + `
		SELECT {{.AllColumns ""}}
		FROM {{.Table}}
		WHERE {{.PK.Column}} = $1
	` + "`" + `

	row := db.QueryRowContext(ctx, query, id)
	var m {{.ModelPkgAlias}}.{{.Name}}
	if err := row.Scan({{.AllScanArgs "m"}}); err != nil {
		return nil, fmt.Errorf("get{{.Name}}ByID(%v): %w", id, err)
	}

	{{if .Relations}}
	if cfg.PreloadAssociations {
		if err := preload{{.Name}}Associations(ctx, db, []*{{.ModelPkgAlias}}.{{.Name}}{&m}); err != nil {
			return nil, fmt.Errorf("get{{.Name}}ByID(%v): preloading associations: %w", id, err)
		}
	}
	{{end}}

	return &m, nil
}

// Exists{{.Name}}ByID reports whether a {{.Name}} record with the given primary key exists.
func Exists{{.Name}}ByID(ctx context.Context, db DBTX, id {{.PK.Type}}) (bool, error) {
	if db == nil {
		return false, errors.New("exists{{.Name}}ByID: db is nil")
	}

	const query = ` + "`" + `SELECT EXISTS(SELECT 1 FROM {{.Table}} WHERE {{.PK.Column}} = $1)` + "`" + `

	var exists bool
	if err := db.QueryRowContext(ctx, query, id).Scan(&exists); err != nil {
		return false, fmt.Errorf("exists{{.Name}}ByID(%v): %w", id, err)
	}
	return exists, nil
}

// Count{{.Name}}s returns total records matching optional filter criteria.
func Count{{.Name}}s(ctx context.Context, db DBTX, opts ...QueryOption) (int64, error) {
	if db == nil {
		return 0, errors.New("count{{.Name}}s: db is nil")
	}

	clause, args, _ := applyQueryOptions("", opts...)
	query := "SELECT COUNT(*) FROM {{.Table}}" + clause

	var count int64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count{{.Name}}s: %w", err)
	}
	return count, nil
}

// FetchAll{{.Name}}s retrieves a filtered/paginated slice of {{.Name}} records.
func FetchAll{{.Name}}s(ctx context.Context, db DBTX, opts ...QueryOption) ([]*{{.ModelPkgAlias}}.{{.Name}}, error) {
	if db == nil {
		return nil, errors.New("fetchAll{{.Name}}s: db is nil")
	}

	clause, args, cfg := applyQueryOptions("{{.PK.Column}}", opts...)
	query := "SELECT {{.AllColumns ""}} FROM {{.Table}}" + clause

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fetchAll{{.Name}}s: %w", err)
	}
	defer rows.Close()

	var items []*{{.ModelPkgAlias}}.{{.Name}}
	for rows.Next() {
		var m {{.ModelPkgAlias}}.{{.Name}}
		if err := rows.Scan({{.AllScanArgs "m"}}); err != nil {
			return nil, fmt.Errorf("fetchAll{{.Name}}s: scanning row: %w", err)
		}
		items = append(items, &m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetchAll{{.Name}}s: iterating rows: %w", err)
	}

	{{if .Relations}}
	if cfg.PreloadAssociations && len(items) > 0 {
		if err := preload{{.Name}}Associations(ctx, db, items); err != nil {
			return nil, fmt.Errorf("fetchAll{{.Name}}s: preloading associations: %w", err)
		}
	}
	{{end}}

	return items, nil
}

{{if .Relations}}
func preload{{.Name}}Associations(ctx context.Context, db DBTX, items []*{{.ModelPkgAlias}}.{{.Name}}) error {
	if len(items) == 0 {
		return nil
	}

{{range .Relations}}
{{$target := index $.AllKnownModels .TargetModel}}

{{if eq .Type "HasMany"}}
	// Preload {{.FieldName}} (HasMany)
	{
		parentMap := make(map[{{$.PK.Type}}]*{{$.ModelPkgAlias}}.{{$.Name}}, len(items))
		pkArgs := make([]any, 0, len(items))
		placeholders := make([]string, 0, len(items))

		for _, item := range items {
			if item != nil {
				parentMap[item.{{$.PK.Name}}] = item
				pkArgs = append(pkArgs, item.{{$.PK.Name}})
				placeholders = append(placeholders, fmt.Sprintf("$%d", len(pkArgs)))
			}
		}

		if len(pkArgs) > 0 {
			query := fmt.Sprintf(
				"SELECT {{$target.AllColumns ""}} FROM {{$target.Table}} WHERE {{.ForeignKey}} IN (%s)",
				strings.Join(placeholders, ", "),
			)
			rows, err := db.QueryContext(ctx, query, pkArgs...)
			if err != nil {
				return fmt.Errorf("preload {{$.Name}}.{{.FieldName}}: %w", err)
			}
			defer rows.Close()

			for rows.Next() {
				var child {{$.ModelPkgAlias}}.{{$target.Name}}
				if err := rows.Scan({{$target.AllScanArgs "child"}}); err != nil {
					return fmt.Errorf("preload {{$.Name}}.{{.FieldName}} scan: %w", err)
				}
				{{$childFKField := $target.FieldByColumn .ForeignKey}}
				{{if $childFKField}}
				if parent, ok := parentMap[{{if $childFKField.Nullable}}*{{end}}child.{{$childFKField.Name}}]; ok {
					parent.{{.FieldName}} = append(parent.{{.FieldName}}, child)
				}
				{{end}}
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("preload {{$.Name}}.{{.FieldName}} iteration: %w", err)
			}
		}
	}
{{end}}

{{if eq .Type "BelongsTo"}}
	// Preload {{.FieldName}} (BelongsTo)
	{
		{{$parentFKField := $.FieldByColumn .ForeignKey}}
		{{if $parentFKField}}
		fkMap := make(map[{{$target.PK.Type}}][]*{{$.ModelPkgAlias}}.{{$.Name}})
		fkArgs := make([]any, 0, len(items))
		placeholders := make([]string, 0, len(items))
		seenFK := make(map[{{$target.PK.Type}}]bool)

		for _, item := range items {
			if item == nil {
				continue
			}
			fkVal := item.{{$parentFKField.Name}}
			{{if $parentFKField.Nullable}}
			if fkVal == nil {
				continue
			}
			val := *fkVal
			{{else}}
			val := fkVal
			{{end}}
			if isZero(val) {
				continue
			}
			fkMap[val] = append(fkMap[val], item)
			if !seenFK[val] {
				seenFK[val] = true
				fkArgs = append(fkArgs, val)
				placeholders = append(placeholders, fmt.Sprintf("$%d", len(fkArgs)))
			}
		}

		if len(fkArgs) > 0 {
			query := fmt.Sprintf(
				"SELECT {{$target.AllColumns ""}} FROM {{$target.Table}} WHERE {{$target.PK.Column}} IN (%s)",
				strings.Join(placeholders, ", "),
			)
			rows, err := db.QueryContext(ctx, query, fkArgs...)
			if err != nil {
				return fmt.Errorf("preload {{$.Name}}.{{.FieldName}}: %w", err)
			}
			defer rows.Close()

			for rows.Next() {
				var target {{$.ModelPkgAlias}}.{{$target.Name}}
				if err := rows.Scan({{$target.AllScanArgs "target"}}); err != nil {
					return fmt.Errorf("preload {{$.Name}}.{{.FieldName}} scan: %w", err)
				}
				if parents, ok := fkMap[target.{{$target.PK.Name}}]; ok {
					for _, parent := range parents {
						{{if .IsPointer}}
						targetCopy := target
						parent.{{.FieldName}} = &targetCopy
						{{else}}
						parent.{{.FieldName}} = target
						{{end}}
					}
				}
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("preload {{$.Name}}.{{.FieldName}} iteration: %w", err)
			}
		}
		{{end}}
	}
{{end}}
{{end}}

	return nil
}
{{end}}

// Update{{.Name}} updates an existing {{.Name}} record in {{.Table}}.
func Update{{.Name}}(ctx context.Context, db DBTX, m *{{.ModelPkgAlias}}.{{.Name}}) error {
	if db == nil {
		return errors.New("update{{.Name}}: db is nil")
	}
	if m == nil {
		return errors.New("update{{.Name}}: m is nil")
	}

	const query = ` + "`" + `
		UPDATE {{.Table}}
		SET {{.UpdateSetClause}}
		WHERE {{.PK.Column}} = ${{.UpdatePKPlaceholderIdx}}
	` + "`" + `

	res, err := db.ExecContext(ctx, query, {{.UpdatableScanArgs "m"}}, m.{{.PK.Name}})
	if err != nil {
		return fmt.Errorf("update{{.Name}}(%v): %w", m.{{.PK.Name}}, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return fmt.Errorf("update{{.Name}}(%v): %w", m.{{.PK.Name}}, sql.ErrNoRows)
	}
	return nil
}

// Delete{{.Name}} deletes the {{.Name}} record identified by id from {{.Table}}.
func Delete{{.Name}}(ctx context.Context, db DBTX, id {{.PK.Type}}) error {
	if db == nil {
		return errors.New("delete{{.Name}}: db is nil")
	}

	const query = ` + "`" + `DELETE FROM {{.Table}} WHERE {{.PK.Column}} = $1` + "`" + `

	res, err := db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("delete{{.Name}}(%v): %w", id, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return fmt.Errorf("delete{{.Name}}(%v): %w", id, sql.ErrNoRows)
	}
	return nil
}
`

func generateQueries(m Model) (string, error) {
	tmpl, err := template.New("gen").Parse(codeTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing code template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, m); err != nil {
		return "", fmt.Errorf("executing code template for %q: %w", m.Name, err)
	}

	return buf.String(), nil
}

func toSnakeCase(str string) string {
	var b strings.Builder
	runes := []rune(str)
	length := len(runes)

	for i := range length {
		r := runes[i]
		if unicode.IsUpper(r) {
			if i > 0 {
				prev := runes[i-1]
				nextIsLower := i+1 < length && unicode.IsLower(runes[i+1])
				if unicode.IsLower(prev) || (nextIsLower && unicode.IsUpper(prev)) {
					b.WriteRune('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
