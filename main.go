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
	FieldName   string       // Struct field name on the parent, e.g. "User" or "Orders".
	TargetModel string       // Related model type name, e.g. "User" or "Order".
	Type        RelationType // Cardinality type: HasMany or BelongsTo.
	ForeignKey  string       // Column name storing the reference ID.
	References  string       // Column name being referenced.
	IsPointer   bool         // True if the field is a pointer (*User vs User).
}

// Field describes a single scalar struct field mapped to a database column,
// including its nullability and read/write permission derived from GORM tags.
type Field struct {
	Name       string     // Go struct field name, e.g. "Email".
	Type       string     // Go type as written in source, e.g. "string" or "*string".
	Column     string     // Database column name, e.g. "email".
	IsPK       bool       // True if this field is the primary key.
	IsIgnore   bool       // True if the field is excluded entirely (gorm:"-").
	Nullable   bool       // True if the column may return SQL NULL (gorm:"null" or a pointer type).
	Permission Permission // Read/write visibility permission.
	HasDefault bool       // True if the field tag contains a default clause.
}

// IsPointer reports whether the field is a pointer type.
func (f Field) IsPointer() bool {
	return strings.HasPrefix(f.Type, "*")
}

// IsTimestamp reports whether the field represents a time.Time struct.
func (f Field) IsTimestamp() bool {
	return f.BaseType() == "time.Time"
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
	Package        string           // Generated package name, e.g. "queries".
	ModelPkg       string           // Full import path of the source models package.
	ModelPkgAlias  string           // Import alias for the source models package, e.g. "models".
	Name           string           // Go type name, e.g. "User".
	Table          string           // Database table name, e.g. "users".
	Fields         []Field          // List of scalar database fields.
	PK             *Field           // Primary key field reference.
	Relations      []Relation       // Discovered HasMany and BelongsTo relations.
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
	var sb strings.Builder
	sb.Grow(len(fields) * 16)

	for i, f := range fields {
		if i > 0 {
			sb.WriteString(", ")
		}
		if prefix != "" {
			sb.WriteString(prefix)
			sb.WriteByte('.')
			sb.WriteString(f.Column)
		} else {
			sb.WriteString(f.Column)
		}
	}
	return sb.String()
}

func (m Model) InsertColumns() string {
	fields := m.WritableFields()
	var sb strings.Builder
	sb.Grow(len(fields) * 12)
	for i, f := range fields {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(f.Column)
	}
	return sb.String()
}

func (m Model) InsertPlaceholders() string {
	fields := m.WritableFields()
	var sb strings.Builder
	sb.Grow(len(fields) * 4)
	for i := range fields {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "$%d", i+1)
	}
	return sb.String()
}

func (m Model) UpdateSetClause() string {
	fields := m.UpdatableFields()
	var sb strings.Builder
	sb.Grow(len(fields) * 16)
	for i, f := range fields {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s = $%d", f.Column, i+1)
	}
	return sb.String()
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

// AllRelationColumns returns a comma-separated list of SELECT columns for all model relations.
func (m Model) AllRelationColumns() string {
	var sb strings.Builder
	for i, rel := range m.Relations {
		alias := fmt.Sprintf("r%d", i)
		target, ok := m.AllKnownModels[rel.TargetModel]
		if !ok {
			continue
		}
		for _, f := range target.SelectableFields() {
			sb.WriteString(", ")
			sb.WriteString(alias)
			sb.WriteByte('.')
			sb.WriteString(f.Column)
		}
	}
	return sb.String()
}

// AllRelationJoins returns the combined LEFT JOIN SQL statements for all model relations.
func (m Model) AllRelationJoins(parentPrefix string) string {
	var joins []string
	for i, rel := range m.Relations {
		alias := fmt.Sprintf("r%d", i)
		target, ok := m.AllKnownModels[rel.TargetModel]
		if !ok {
			continue
		}
		switch rel.Type {
		case RelHasMany:
			joins = append(joins, fmt.Sprintf("LEFT JOIN %s %s ON %s.%s = %s.%s",
				target.Table, alias, alias, rel.ForeignKey, parentPrefix, m.PK.Column))
		case RelBelongsTo:
			joins = append(joins, fmt.Sprintf("LEFT JOIN %s %s ON %s.%s = %s.%s",
				target.Table, alias, alias, target.PK.Column, parentPrefix, rel.ForeignKey))
		}
	}
	return strings.Join(joins, "\n\t\t")
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

	fieldType := field.Type

	// Detect HasMany (slice) vs BelongsTo (scalar)
	if arrayType, ok := fieldType.(*ast.ArrayType); ok {
		rel.Type = RelHasMany
		fieldType = arrayType.Elt
	} else {
		rel.Type = RelBelongsTo
	}

	// Detect if pointer
	if starExpr, ok := fieldType.(*ast.StarExpr); ok {
		rel.IsPointer = true
		fieldType = starExpr.X
	}

	// Relations within the same package are Identifiers (e.g. User or Order).
	// Imported types like time.Time or uuid.UUID are SelectorExpr and NOT model relations.
	ident, ok := fieldType.(*ast.Ident)
	if !ok || !ident.IsExported() {
		return rel, false
	}
	rel.TargetModel = ident.Name

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

	// First pass: look for column tag explicitly so it overrides default toSnakeCase(fieldName)
	for part := range strings.SplitSeq(gormTag, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), "column:") {
			f.Column = part[len("column:"):]
			break
		}
	}

	for part := range strings.SplitSeq(gormTag, ";") {
		part = strings.TrimSpace(part)
		lower := strings.ToLower(part)

		switch {
		case part == "-":
			f.IsIgnore = true
			return f

		case lower == "primarykey" || lower == "primary_key":
			f.IsPK = true

		case strings.HasPrefix(lower, "default:"):
			f.HasDefault = true

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
	"math"
	"reflect"
	"strings"
	"time"
)

type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// QueryOptions provides optional filtering, ordering, grouping, having, pagination, and association preloading for queries.
type QueryOptions struct {
	Where               string
	Args                []any
	Having              string
	OrderBy             string
	GroupBy             string
	Limit               int
	Offset              int
	PreloadAssociations bool
}

type QueryOption func(*QueryOptions)

func Where(where string, args ...any) QueryOption {
	return func(o *QueryOptions) {
		if o.Where != "" {
			o.Where += " AND " + where
		} else {
			o.Where = where
		}
		o.Args = append(o.Args, args...)
	}
}

// ILIKE performs a case-insensitive pattern match on a column using PostgreSQL ILIKE.
// It wraps the provided value with wildcard characters ("%<value>%").
// It does nothing if value is empty.
func ILIKE(column, value string) QueryOption {
	return func(o *QueryOptions) {
		if value == "" {
			return
		}

		o.Args = append(o.Args, "%"+value+"%")
		clause := fmt.Sprintf("%s LIKE $%d", column, len(o.Args))

		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

// In filters a column matching any of the provided values.
// It does nothing if values is empty.
func In(column string, values ...any) QueryOption {
	return func(o *QueryOptions) {
		if len(values) == 0 {
			return
		}

		placeholders := make([]string, 0, len(values))
		for _, v := range values {
			o.Args = append(o.Args, v)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(o.Args)))
		}

		clause := fmt.Sprintf("%s IN (%s)", column, strings.Join(placeholders, ", "))
		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

// NotIn filters a column not matching any of the provided values.
// It does nothing if values is empty.
func NotIn(column string, values ...any) QueryOption {
	return func(o *QueryOptions) {
		if len(values) == 0 {
			return
		}

		placeholders := make([]string, 0, len(values))
		for _, v := range values {
			o.Args = append(o.Args, v)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(o.Args)))
		}

		clause := fmt.Sprintf("%s NOT IN (%s)", column, strings.Join(placeholders, ", "))
		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

// IsNull filters rows where the column is NULL.
func IsNull(column string) QueryOption {
	return func(o *QueryOptions) {
		clause := fmt.Sprintf("%s IS NULL", column)
		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

// IsNotNull filters rows where the column is NOT NULL.
func IsNotNull(column string) QueryOption {
	return func(o *QueryOptions) {
		clause := fmt.Sprintf("%s IS NOT NULL", column)
		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

// Between applies a BETWEEN min AND max filter on a column.
func Between(column string, min, max any) QueryOption {
	return func(o *QueryOptions) {
		o.Args = append(o.Args, min, max)
		p1 := fmt.Sprintf("$%d", len(o.Args)-1)
		p2 := fmt.Sprintf("$%d", len(o.Args))

		clause := fmt.Sprintf("%s BETWEEN %s AND %s", column, p1, p2)
		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

// Search performs a pattern match across multiple columns.
// It does nothing if query or columns is empty.
func Search(query string, columns ...string) QueryOption {
	return func(o *QueryOptions) {
		if query == "" || len(columns) == 0 {
			return
		}

		o.Args = append(o.Args, "%"+query+"%")
		placeholder := fmt.Sprintf("$%d", len(o.Args))

		clauses := make([]string, 0, len(columns))
		for _, col := range columns {
			clauses = append(clauses, fmt.Sprintf("%s LIKE %s", col, placeholder))
		}

		groupClause := "(" + strings.Join(clauses, " OR ") + ")"
		if o.Where != "" {
			o.Where += " AND " + groupClause
		} else {
			o.Where = groupClause
		}
	}
}

// Gt applies a greater-than filter (>).
func Gt(column string, val any) QueryOption {
	return func(o *QueryOptions) {
		o.Args = append(o.Args, val)
		clause := fmt.Sprintf("%s > $%d", column, len(o.Args))
		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

// Gte applies a greater-than-or-equal filter (>=).
func Gte(column string, val any) QueryOption {
	return func(o *QueryOptions) {
		o.Args = append(o.Args, val)
		clause := fmt.Sprintf("%s >= $%d", column, len(o.Args))
		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

// Lt applies a less-than filter (<).
func Lt(column string, val any) QueryOption {
	return func(o *QueryOptions) {
		o.Args = append(o.Args, val)
		clause := fmt.Sprintf("%s < $%d", column, len(o.Args))
		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

// Lte applies a less-than-or-equal filter (<=).
func Lte(column string, val any) QueryOption {
	return func(o *QueryOptions) {
		o.Args = append(o.Args, val)
		clause := fmt.Sprintf("%s <= $%d", column, len(o.Args))
		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

func Having(having string, args ...any) QueryOption {
	return func(o *QueryOptions) {
		if o.Having != "" {
			o.Having += " AND " + having
		} else {
			o.Having = having
		}
		o.Args = append(o.Args, args...)
	}
}

func OrderBy(orderBy string) QueryOption {
	return func(o *QueryOptions) {
		o.OrderBy = orderBy
	}
}

func GroupBy(groupBy string) QueryOption {
	return func(o *QueryOptions) {
		o.GroupBy = groupBy
	}
}

func Limit(limit int) QueryOption {
	return func(o *QueryOptions) {
		o.Limit = limit
	}
}

func Offset(offset int) QueryOption {
	return func(o *QueryOptions) {
		o.Offset = offset
	}
}

// WithPreloadAssociations enables or disables preloading associated model relations.
func PreloadAssociations(preload bool) QueryOption {
	return func(o *QueryOptions) {
		o.PreloadAssociations = preload
	}
}

// DateRange applies date range filter on a date column.
// e.g DateRange("DATE(created_at)", "2021-01-01", "2021-12-31")
// It does nothing if start or end is empty.
func DateRange(column string, start, end string) QueryOption {
	return func(o *QueryOptions) {
		if start != "" && end != "" {
			o.Args = append(o.Args, start, end)
			placeholder1 := fmt.Sprintf("$%d", len(o.Args)-1)
			placeholder2 := fmt.Sprintf("$%d", len(o.Args))
			clause := fmt.Sprintf("%s BETWEEN %s AND %s", column, placeholder1, placeholder2)
			if o.Where != "" {
				o.Where += " AND " + clause
			} else {
				o.Where = clause
			}
		} else if start != "" {
			o.Args = append(o.Args, start)
			clause := fmt.Sprintf("%s >= $%d", column, len(o.Args))
			if o.Where != "" {
				o.Where += " AND " + clause
			} else {
				o.Where = clause
			}
		} else if end != "" {
			o.Args = append(o.Args, end)
			clause := fmt.Sprintf("%s <= $%d", column, len(o.Args))
			if o.Where != "" {
				o.Where += " AND " + clause
			} else {
				o.Where = clause
			}
		}
	}
}

// MonthRange is the same as DateRange but truncates the date to month.
// e.g MonthRange("DATE(created_at)", "2021-01-01", "2021-12-31")
// It does nothing if start or end is empty.
func MonthRange(column string, start, end string) QueryOption {
	return func(o *QueryOptions) {
		if start != "" && end != "" {
			o.Args = append(o.Args, start, end)
			p1 := fmt.Sprintf("$%d", len(o.Args)-1)
			p2 := fmt.Sprintf("$%d", len(o.Args))
			clause := fmt.Sprintf("%s BETWEEN DATE_TRUNC('month', %s::DATE) AND DATE_TRUNC('month', %s::DATE)", column, p1, p2)
			if o.Where != "" {
				o.Where += " AND " + clause
			} else {
				o.Where = clause
			}
		} else if start != "" {
			o.Args = append(o.Args, start)
			clause := fmt.Sprintf("%s >= DATE_TRUNC('month', $%d::DATE)", column, len(o.Args))
			if o.Where != "" {
				o.Where += " AND " + clause
			} else {
				o.Where = clause
			}
		} else if end != "" {
			o.Args = append(o.Args, end)
			clause := fmt.Sprintf("%s <= DATE_TRUNC('month', $%d::DATE)", column, len(o.Args))
			if o.Where != "" {
				o.Where += " AND " + clause
			} else {
				o.Where = clause
			}
		}
	}
}

// YearRange is the same as date range but truncates the date to year.
// e.g YearRange("DATE(created_at)", "2021-01-01", "2024-12-31")
// It does nothing if start or end is empty.
func YearRange(column string, start, end string) QueryOption {
	return func(o *QueryOptions) {
		if start != "" && end != "" {
			o.Args = append(o.Args, start, end)
			p1 := fmt.Sprintf("$%d", len(o.Args)-1)
			p2 := fmt.Sprintf("$%d", len(o.Args))
			clause := fmt.Sprintf("%s BETWEEN DATE_TRUNC('year', %s::DATE) AND DATE_TRUNC('year', %s::DATE)", column, p1, p2)
			if o.Where != "" {
				o.Where += " AND " + clause
			} else {
				o.Where = clause
			}
		} else if start != "" {
			o.Args = append(o.Args, start)
			clause := fmt.Sprintf("%s >= DATE_TRUNC('year', $%d::DATE)", column, len(o.Args))
			if o.Where != "" {
				o.Where += " AND " + clause
			} else {
				o.Where = clause
			}
		} else if end != "" {
			o.Args = append(o.Args, end)
			clause := fmt.Sprintf("%s <= DATE_TRUNC('year', $%d::DATE)", column, len(o.Args))
			if o.Where != "" {
				o.Where += " AND " + clause
			} else {
				o.Where = clause
			}
		}
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

	if cfg.Having != "" {
		sb.WriteString(" HAVING ")
		sb.WriteString(cfg.Having)
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

// PaginationResult holds the output of a paginated query.
type PaginationResult[T any] struct {
	Page       int   ` + "`" + `json:"page"` + "`" + `
	PageSize   int   ` + "`" + `json:"page_size"` + "`" + `
	TotalPages int64 ` + "`" + `json:"total_pages"` + "`" + `
	Count      int64 ` + "`" + `json:"count"` + "`" + `
	HasNext    bool  ` + "`" + `json:"has_next"` + "`" + `
	HasPrev    bool  ` + "`" + `json:"has_prev"` + "`" + `
	Results    []T   ` + "`" + `json:"results"` + "`" + `
}

// FetchFunc defines a function signature capable of fetching records using DBTX and QueryOptions.
type FetchFunc[T any] func(ctx context.Context, db DBTX, opts ...QueryOption) ([]T, error)

// CountFunc defines a function signature capable of counting total matching rows using DBTX and QueryOptions.
type CountFunc func(ctx context.Context, db DBTX, opts ...QueryOption) (int64, error)

// Paginate executes a paginated query using DBTX, counting total rows and retrieving the requested page.
func Paginate[T any](ctx context.Context, db DBTX, countFn CountFunc, fetchFn FetchFunc[T], page, pageSize int, opts ...QueryOption) (*PaginationResult[T], error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 10
	}

	totalCount, err := countFn(ctx, db, opts...)
	if err != nil {
		return nil, fmt.Errorf("paginate: counting total rows: %w", err)
	}

	// Append pagination limit and offset options
	pOpts := append([]QueryOption(nil), opts...)
	pOpts = append(pOpts, Limit(pageSize), Offset((page-1)*pageSize))

	results, err := fetchFn(ctx, db, pOpts...)
	if err != nil {
		return nil, fmt.Errorf("paginate: fetching page results: %w", err)
	}

	if results == nil {
		results = make([]T, 0)
	}

	return &PaginationResult[T]{
		Page:       page,
		PageSize:   pageSize,
		HasNext:    int64(page*pageSize) < totalCount,
		HasPrev:    page > 1,
		Results:    results,
		Count:      totalCount,
		TotalPages: int64(math.Ceil(float64(totalCount) / float64(pageSize))),
	}, nil
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

	if v, ok := src.(T); ok {
		*n.dst = v
		return nil
	}

	if scanner, ok := any(n.dst).(sql.Scanner); ok {
		return scanner.Scan(src)
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

	switch p := any(dst).(type) {
	case *string:
		switch s := src.(type) {
		case []byte:
			*p = string(s)
			return nil
		case string:
			*p = s
			return nil
		}
	case *[]byte:
		switch s := src.(type) {
		case string:
			*p = []byte(s)
			return nil
		case []byte:
			cp := make([]byte, len(s))
			copy(cp, s)
			*p = cp
			return nil
		}
	case *time.Time:
		switch s := src.(type) {
		case string:
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return fmt.Errorf("convertAssign: failed to parse time string %q: %w", s, err)
			}
			*p = t
			return nil
		case []byte:
			t, err := time.Parse(time.RFC3339, string(s))
			if err != nil {
				return fmt.Errorf("convertAssign: failed to parse time bytes %q: %w", s, err)
			}
			*p = t
			return nil
		}
	case *int:
		if i, ok := toInt64(src); ok {
			*p = int(i)
			return nil
		}
	case *int8:
		if i, ok := toInt64(src); ok {
			*p = int8(i)
			return nil
		}
	case *int16:
		if i, ok := toInt64(src); ok {
			*p = int16(i)
			return nil
		}
	case *int32:
		if i, ok := toInt64(src); ok {
			*p = int32(i)
			return nil
		}
	case *int64:
		if i, ok := toInt64(src); ok {
			*p = i
			return nil
		}
	case *uint:
		if u, ok := toUint64(src); ok {
			*p = uint(u)
			return nil
		}
	case *uint8:
		if u, ok := toUint64(src); ok {
			*p = uint8(u)
			return nil
		}
	case *uint16:
		if u, ok := toUint64(src); ok {
			*p = uint16(u)
			return nil
		}
	case *uint32:
		if u, ok := toUint64(src); ok {
			*p = uint32(u)
			return nil
		}
	case *uint64:
		if u, ok := toUint64(src); ok {
			*p = u
			return nil
		}
	case *float32:
		if f, ok := toFloat64(src); ok {
			*p = float32(f)
			return nil
		}
	case *float64:
		if f, ok := toFloat64(src); ok {
			*p = f
			return nil
		}
	}

	vDst := reflect.ValueOf(dst).Elem()
	vSrc := reflect.ValueOf(src)

	if vSrc.Type().ConvertibleTo(vDst.Type()) {
		vDst.Set(vSrc.Convert(vDst.Type()))
		return nil
	}

	return fmt.Errorf("scanNullable: cannot convert %T (%v) to %T", src, src, dst)
}

func toInt64(src any) (int64, bool) {
	switch v := src.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), true
	case uint8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		return int64(v), true
	default:
		return 0, false
	}
}

func toUint64(src any) (uint64, bool) {
	switch v := src.(type) {
	case int:
		return uint64(v), true
	case int8:
		return uint64(v), true
	case int16:
		return uint64(v), true
	case int32:
		return uint64(v), true
	case int64:
		return uint64(v), true
	case uint:
		return uint64(v), true
	case uint8:
		return uint64(v), true
	case uint16:
		return uint64(v), true
	case uint32:
		return uint64(v), true
	case uint64:
		return v, true
	default:
		return 0, false
	}
}

func toFloat64(src any) (float64, bool) {
	switch v := src.(type) {
	case float32:
		return float64(v), true
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	default:
		return 0, false
	}
}

func IsZero(v any) bool {
	if v == nil {
		return true
	}

	switch t := v.(type) {
	case time.Time:
		return t.IsZero()
	case *time.Time:
		return t == nil || t.IsZero()
	}

	val := reflect.ValueOf(v)
	if val.Kind() == reflect.Pointer {
		if val.IsNil() {
			return true
		}
		return val.Elem().IsZero()
	}

	return val.IsZero()
}

func toPtr[T any](v T) *T {
	if IsZero(v) {
		return nil
	}
	return &v
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

	fields := []struct {
		col        string
		val        any
		omitIfZero bool
	}{
		{{range .WritableFields}}{col: "{{.Column}}", val: m.{{.Name}}, omitIfZero: {{or .HasDefault (and .IsTimestamp (not .Nullable))}}},
		{{end}}
	}

	cols := make([]string, 0, len(fields))
	args := make([]any, 0, len(fields))
	placeholders := make([]string, 0, len(fields))

	for _, f := range fields {
		if f.omitIfZero && IsZero(f.val) {
			continue
		}
		cols = append(cols, f.col)
		args = append(args, f.val)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}

	var query string
	if len(cols) == 0 {
		query = "INSERT INTO {{.Table}} DEFAULT VALUES RETURNING {{.AllColumns ""}}"
	} else {
		query = fmt.Sprintf("INSERT INTO {{.Table}} (%s) VALUES (%s) RETURNING {{.AllColumns ""}}",
			strings.Join(cols, ", "), strings.Join(placeholders, ", "))
	}

	if err := db.QueryRowContext(ctx, query, args...).Scan({{.AllScanArgs "m"}}); err != nil {
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

	{{if .Relations}}
	if cfg.PreloadAssociations {
		return get{{.Name}}ByIDWithRelations(ctx, db, id, cfg)
	}
	{{end}}

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

	{{if .Relations}}
	if cfg.PreloadAssociations {
		return fetchAll{{.Name}}sWithRelations(ctx, db, clause, args)
	}
	{{end}}

	query := "SELECT {{.AllColumns ""}} FROM {{.Table}}" + clause

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fetchAll{{.Name}}s: %w", err)
	}
	defer rows.Close()

	items := make([]*{{.ModelPkgAlias}}.{{.Name}}, 0, 16)
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

	return items, nil
}

{{if .Relations}}
func get{{.Name}}ByIDWithRelations(ctx context.Context, db DBTX, id {{.PK.Type}}, cfg QueryOptions) (*{{.ModelPkgAlias}}.{{.Name}}, error) {
	const query = ` + "`" + `
		SELECT 
			{{.AllColumns "p"}}{{.AllRelationColumns}}
		FROM {{.Table}} p
		{{.AllRelationJoins "p"}}
		WHERE p.{{.PK.Column}} = $1
	` + "`" + `

	rows, err := db.QueryContext(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("get{{.Name}}ByIDWithRelations(%v): %w", id, err)
	}
	defer rows.Close()

	var parent *{{.ModelPkgAlias}}.{{.Name}}

	{{range $idx, $rel := .Relations}}
	{{$target := index $.AllKnownModels $rel.TargetModel}}
	seen_{{$rel.FieldName}} := make(map[{{$target.PK.Type}}]bool, 4)
	{{end}}

	for rows.Next() {
		var p {{.ModelPkgAlias}}.{{.Name}}

		{{range $idx, $rel := .Relations}}
		{{$target := index $.AllKnownModels $rel.TargetModel}}
		{{range $target.SelectableFields}}var r{{$idx}}_{{.Name}} {{.BaseType}}
		{{end}}
		{{end}}

		scanArgs := []any{
			{{.AllScanArgs "p"}},
			{{range $idx, $rel := .Relations}}
			{{$target := index $.AllKnownModels $rel.TargetModel}}
			{{range $target.SelectableFields}}scanNullable(&r{{$idx}}_{{.Name}}),
			{{end}}
			{{end}}
		}

		if err := rows.Scan(scanArgs...); err != nil {
			return nil, fmt.Errorf("get{{.Name}}ByIDWithRelations(%v): scanning row: %w", id, err)
		}

		if parent == nil {
			parent = &p
		}

		{{range $idx, $rel := .Relations}}
		{{$target := index $.AllKnownModels $rel.TargetModel}}
		rPk{{$idx}} := r{{$idx}}_{{$target.PK.Name}}
		if !IsZero(rPk{{$idx}}) {
			if !seen_{{$rel.FieldName}}[rPk{{$idx}}] {
				seen_{{$rel.FieldName}}[rPk{{$idx}}] = true
				child := {{$.ModelPkgAlias}}.{{$target.Name}}{
					{{range $target.SelectableFields}}{{.Name}}: {{if .IsPointer}}toPtr(r{{$idx}}_{{.Name}}){{else}}r{{$idx}}_{{.Name}}{{end}},
					{{end}}
				}
				{{if eq $rel.Type "HasMany"}}
				parent.{{$rel.FieldName}} = append(parent.{{$rel.FieldName}}, child)
				{{else if eq $rel.Type "BelongsTo"}}
				{{if $rel.IsPointer}}
				parent.{{$rel.FieldName}} = &child
				{{else}}
				parent.{{$rel.FieldName}} = child
				{{end}}
				{{end}}
			}
		}
		{{end}}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get{{.Name}}ByIDWithRelations(%v): iterating rows: %w", id, err)
	}
	if parent == nil {
		return nil, fmt.Errorf("get{{.Name}}ByIDWithRelations(%v): %w", id, sql.ErrNoRows)
	}

	return parent, nil
}

func fetchAll{{.Name}}sWithRelations(ctx context.Context, db DBTX, clause string, args []any) ([]*{{.ModelPkgAlias}}.{{.Name}}, error) {
	query := ` + "`" + `
		WITH p AS (
			SELECT {{.AllColumns "p"}}
			FROM {{.Table}} p
	` + "`" + ` + clause + ` + "`" + `
		)
		SELECT 
			{{.AllColumns "p"}}{{.AllRelationColumns}}
		FROM p
		{{.AllRelationJoins "p"}}
		ORDER BY p.{{.PK.Column}} ASC
	` + "`" + `

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fetchAll{{.Name}}sWithRelations: %w", err)
	}
	defer rows.Close()

	items := make([]*{{.ModelPkgAlias}}.{{.Name}}, 0, 16)
	itemsMap := make(map[{{.PK.Type}}]*{{.ModelPkgAlias}}.{{.Name}}, 16)

	{{range $idx, $rel := .Relations}}
	{{$target := index $.AllKnownModels $rel.TargetModel}}
	seen_{{$rel.FieldName}} := make(map[{{$.PK.Type}}]map[{{$target.PK.Type}}]bool, 16)
	{{end}}

	for rows.Next() {
		var p {{.ModelPkgAlias}}.{{.Name}}

		{{range $idx, $rel := .Relations}}
		{{$target := index $.AllKnownModels $rel.TargetModel}}
		{{range $target.SelectableFields}}var r{{$idx}}_{{.Name}} {{.BaseType}}
		{{end}}
		{{end}}

		scanArgs := []any{
			{{.AllScanArgs "p"}},
			{{range $idx, $rel := .Relations}}
			{{$target := index $.AllKnownModels $rel.TargetModel}}
			{{range $target.SelectableFields}}scanNullable(&r{{$idx}}_{{.Name}}),
			{{end}}
			{{end}}
		}

		if err := rows.Scan(scanArgs...); err != nil {
			return nil, fmt.Errorf("fetchAll{{.Name}}sWithRelations: scanning row: %w", err)
		}

		pPK := p.{{.PK.Name}}
		parent, exists := itemsMap[pPK]
		if !exists {
			parent = &p
			itemsMap[pPK] = parent
			items = append(items, parent)

			{{range $idx, $rel := .Relations}}
			{{$target := index $.AllKnownModels $rel.TargetModel}}
			seen_{{$rel.FieldName}}[pPK] = make(map[{{$target.PK.Type}}]bool, 4)
			{{end}}
		}

		{{range $idx, $rel := .Relations}}
		{{$target := index $.AllKnownModels $rel.TargetModel}}
		rPk{{$idx}} := r{{$idx}}_{{$target.PK.Name}}
		if !IsZero(rPk{{$idx}}) {
			if !seen_{{$rel.FieldName}}[pPK][rPk{{$idx}}] {
				seen_{{$rel.FieldName}}[pPK][rPk{{$idx}}] = true
				child := {{$.ModelPkgAlias}}.{{$target.Name}}{
					{{range $target.SelectableFields}}{{.Name}}: {{if .IsPointer}}toPtr(r{{$idx}}_{{.Name}}){{else}}r{{$idx}}_{{.Name}}{{end}},
					{{end}}
				}
				{{if eq $rel.Type "HasMany"}}
				parent.{{$rel.FieldName}} = append(parent.{{$rel.FieldName}}, child)
				{{else if eq $rel.Type "BelongsTo"}}
				{{if $rel.IsPointer}}
				parent.{{$rel.FieldName}} = &child
				{{else}}
				parent.{{$rel.FieldName}} = child
				{{end}}
				{{end}}
			}
		}
		{{end}}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetchAll{{.Name}}sWithRelations: iterating rows: %w", err)
	}

	return items, nil
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
		return fmt.Errorf("detect rows affected: %w", err)
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
		return fmt.Errorf("detect rows affected: %w", err)
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
