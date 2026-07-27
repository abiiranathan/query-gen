package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
	OnDelete    string       // Referential action from gorm constraint tag, e.g. "CASCADE", "SET NULL". Empty means database default.
	OnUpdate    string       // Referential action from gorm constraint tag, e.g. "CASCADE", "RESTRICT". Empty means database default.
}

// Field describes a single scalar struct field mapped to a database column,
// including its nullability and read/write permission derived from GORM tags.
type Field struct {
	Name            string     // Go struct field name, e.g. "Email".
	Type            string     // Go type as written in source, e.g. "string" or "*string".
	UnderlyingType  string     // Underlying Go primitive type, e.g. "uint64" or "*uint64".
	Column          string     // Database column name, e.g. "email".
	IsPK            bool       // True if this field is a primary key.
	IsIgnore        bool       // True if the field is excluded entirely (gorm:"-").
	Nullable        bool       // True if the column may return SQL NULL (gorm:"null" or a pointer type).
	Permission      Permission // Read/write visibility permission.
	HasDefault      bool       // True if the field tag contains a default clause.
	DefaultVal      string     // Literal default value extracted from gorm tag.
	IsUnique        bool       // True if unique constraint is applied.
	HasIndex        bool       // True if field is indexed.
	IndexName       string     // Custom index name, if specified.
	HasUniqueIndex  bool       // True if field has unique index.
	UniqueIndexName string     // Custom unique index name, if specified.
	RawSQLType      string     // Explicit column type override from gorm:"type:...", e.g. "varchar(255)" or "jsonb". Empty means infer from Go type.
	Size            int        // Column size from gorm:"size:...", e.g. 255 for VARCHAR(255). Zero means unspecified.
	Precision       int        // Numeric precision from gorm:"precision:...". Zero means unspecified.
	Scale           int        // Numeric scale from gorm:"scale:...". Zero means unspecified.
	CheckConstraint string     // Column-level CHECK expression from gorm:"check:expr". Empty means none.
}

// UnderlyingBaseType returns the underlying primitive Go type with pointer indirections stripped.
// E.g., "*permissions.Permission" (where Permission = uint64) -> "uint64".
func (f Field) UnderlyingBaseType() string {
	if f.UnderlyingType != "" {
		return strings.TrimPrefix(f.UnderlyingType, "*")
	}
	return strings.TrimPrefix(f.Type, "*")
}

// IsPointer reports whether the field is a pointer type.
func (f Field) IsPointer() bool {
	return strings.HasPrefix(f.Type, "*")
}

// IsTimestamp reports whether the field represents a time.Time struct.
func (f Field) IsTimestamp() bool {
	return f.UnderlyingBaseType() == "time.Time"
}

// IsDeletedAt reports whether this field represents a soft-delete timestamp.
func (f Field) IsDeletedAt() bool {
	return strings.EqualFold(f.Name, "DeletedAt") || f.Column == "deleted_at"
}

// Writable reports whether this field should appear in an INSERT statement.
// If isCompositePK is true, primary key fields are included because composite
// keys must be explicitly supplied on insert. Single primary key fields are
// omitted so the database can auto-generate their sequence/identity values.
func (f Field) Writable(isCompositePK bool) bool {
	if f.IsPK {
		return isCompositePK
	}
	if f.IsDeletedAt() {
		return false
	}
	return f.Permission != PermReadOnly && f.Permission != PermUpdateOnly
}

// Updatable reports whether this field should appear in an UPDATE statement SET clause.
// Primary key fields are always excluded because they form the WHERE clause.
func (f Field) Updatable() bool {
	if f.IsDeletedAt() {
		return false
	}
	return !f.IsPK && f.Permission != PermReadOnly && f.Permission != PermCreateOnly
}

// WritableFields returns the slice of fields participating in INSERT statements.
func (m Model) WritableFields() []Field {
	isComposite := len(m.PK) > 1
	fields := make([]Field, 0, len(m.Fields))
	for _, f := range m.Fields {
		if f.Writable(isComposite) {
			fields = append(fields, f)
		}
	}
	return fields
}

// Selectable reports whether this field should appear in a SELECT statement.
func (f Field) Selectable() bool {
	return f.Permission != PermWriteOnly
}

// BulkInsertBatchSize returns the maximum number of model instances per bulk insert query
// to keep total bind parameters safely under SQL variable limits (999).
func (m Model) BulkInsertBatchSize() int {
	numCols := len(m.WritableFields())
	if numCols == 0 {
		return 100
	}
	batchSize := 999 / numCols
	if batchSize < 1 {
		return 1
	}
	return batchSize
}

// builtinTypes lists Go predeclared types and common aliases that never
// need a package qualifier when referenced from a generated package.
var builtinTypes = map[string]bool{
	"bool": true, "string": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true, "byte": true, "rune": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
	"any": true, "error": true, "[]byte": true,
}

// QualifiedType returns f's Go type as it must be written from within a
// generated package: pointer/slice modifiers are preserved, and if the
// underlying named type is neither a builtin nor already package-qualified
// (e.g. time.Time), it is prefixed with pkgAlias so identifiers like Age
// resolve to models.Age instead of an undefined bare Age.
func (f Field) QualifiedType(pkgAlias string) string {
	prefix, base := "", f.Type
	for {
		switch {
		case strings.HasPrefix(base, "*"):
			prefix, base = prefix+"*", base[1:]
			continue
		case strings.HasPrefix(base, "[]"):
			prefix, base = prefix+"[]", base[2:]
			continue
		}
		break
	}
	if pkgAlias == "" || builtinTypes[base] || strings.Contains(base, ".") {
		return prefix + base
	}
	return prefix + pkgAlias + "." + base
}

// QualifiedBaseType is QualifiedType with one leading pointer indirection
// stripped, mirroring BaseType — used for scan-target variable declarations.
func (f Field) QualifiedBaseType(pkgAlias string) string {
	base := strings.TrimPrefix(f.Type, "*")
	tmp := Field{Type: base}
	return tmp.QualifiedType(pkgAlias)
}

// Model describes a parsed Go struct and everything needed to render its
// generated CRUD query file: table name, columns, primary key, and any
// HasMany/BelongsTo relations to other known models.
type Model struct {
	Package        string           // Generated package name, e.g. "queries".
	ModelPkg       string           // Full import path of the source models package.
	ModelPkgAlias  string           // Import alias for the source models package, e.g. "models".
	Name           string           // Go type name, e.g. "User".
	NamePlural     string           // Plural of name.
	Table          string           // Database table name, e.g. "users".
	Fields         []Field          // List of scalar database fields.
	PK             []*Field         // Primary key field references (supports single & composite primary keys).
	Relations      []Relation       // Discovered HasMany and BelongsTo relations.
	AllKnownModels map[string]Model // All models in the parsed package, keyed by type name.
	TableChecks    []checkInfo      // Struct-level CHECK constraints not scoped to a single column.
}

// HasDeletedAt reports whether the model contains a DeletedAt soft-delete field.
func (m Model) HasDeletedAt() bool {
	return m.DeletedAtField() != nil
}

// DeletedAtField returns the Field corresponding to DeletedAt, if present.
func (m Model) DeletedAtField() *Field {
	for i := range m.Fields {
		if strings.EqualFold(m.Fields[i].Name, "DeletedAt") || m.Fields[i].Column == "deleted_at" {
			return &m.Fields[i]
		}
	}
	return nil
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

// --- Composite PK Helper Methods ---

// toLowerCamel converts a Go field name to lower camelCase.
// E.g., "UserID" -> "userID", "ID" -> "id", "TenantCode" -> "tenantCode".
func toLowerCamel(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	i := 0
	for i < len(runes) && unicode.IsUpper(runes[i]) {
		i++
	}
	if i == 0 {
		return s
	}
	if i == len(runes) {
		return strings.ToLower(s)
	}
	if i > 1 {
		for j := 0; j < i-1; j++ {
			runes[j] = unicode.ToLower(runes[j])
		}
	} else {
		runes[0] = unicode.ToLower(runes[0])
	}
	return string(runes)
}

// --- Composite PK Helper Methods ---

// FirstPK returns the first primary key field (or nil).
func (m Model) FirstPK() *Field {
	if len(m.PK) > 0 {
		return m.PK[0]
	}
	return nil
}

// PKColumns returns a comma-separated list of primary key database columns,
// optionally prefixed with prefix (e.g. "p.col1, p.col2").
func (m Model) PKColumns(prefix string) string {
	cols := make([]string, len(m.PK))
	for i, pk := range m.PK {
		if prefix != "" {
			cols[i] = prefix + "." + pk.Column
		} else {
			cols[i] = pk.Column
		}
	}
	return strings.Join(cols, ", ")
}

// PKWhereClause generates a SQL WHERE condition for primary keys starting at parameter index startIdx.
// E.g., startIdx=1 -> "col1 = $1 AND col2 = $2"
func (m Model) PKWhereClause(prefix string, startIdx int) string {
	clauses := make([]string, len(m.PK))
	for i, pk := range m.PK {
		col := pk.Column
		if prefix != "" {
			col = prefix + "." + col
		}
		clauses[i] = fmt.Sprintf("%s = $%d", col, startIdx+i)
	}
	return strings.Join(clauses, " AND ")
}

// PKParams returns Go parameter definitions for primary key fields in function signatures,
// using lower-camelCase parameter names derived from field names.
// E.g., single PK: "id string" or "userID int64", composite PK: "userID int64, roleID int64"
func (m Model) PKParams(pkgAlias string) string {
	params := make([]string, len(m.PK))
	for i, pk := range m.PK {
		params[i] = fmt.Sprintf("%s %s", toLowerCamel(pk.Name), pk.QualifiedType(pkgAlias))
	}
	return strings.Join(params, ", ")
}

// PKArgs returns a comma-separated list of primary key argument names or field accesses.
// If prefix is empty, returns lower-camelCase argument names (e.g. "id" or "userID, roleID").
// If prefix is non-empty (e.g. "m."), returns struct field accesses (e.g. "m.UserID, m.RoleID").
func (m Model) PKArgs(prefix string) string {
	args := make([]string, len(m.PK))
	for i, pk := range m.PK {
		if prefix != "" {
			args[i] = prefix + pk.Name
		} else {
			args[i] = toLowerCamel(pk.Name)
		}
	}
	return strings.Join(args, ", ")
}

// PKZeroCheck returns a Go boolean expression checking if any PK field is zero.
// E.g., "IsZero(m.UserID) || IsZero(m.RoleID)"
func (m Model) PKZeroCheck(varName string) string {
	checks := make([]string, len(m.PK))
	for i, pk := range m.PK {
		checks[i] = fmt.Sprintf("IsZero(%s.%s)", varName, pk.Name)
	}
	return strings.Join(checks, " || ")
}

// PKType returns the Go type used for map keys representing this model's primary key.
func (m Model) PKType(pkgAlias string) string {
	if len(m.PK) == 1 {
		return m.PK[0].QualifiedType(pkgAlias)
	}
	fields := make([]string, len(m.PK))
	for i, pk := range m.PK {
		fields[i] = fmt.Sprintf("%s %s", pk.Name, pk.QualifiedType(pkgAlias))
	}
	return fmt.Sprintf("struct{ %s }", strings.Join(fields, "; "))
}

// PKValue returns a Go expression evaluating to the primary key map key value for varName.
func (m Model) PKValue(varName string, pkgAlias string) string {
	if len(m.PK) == 1 {
		return fmt.Sprintf("%s.%s", varName, m.PK[0].Name)
	}
	fields := make([]string, len(m.PK))
	for i, pk := range m.PK {
		fields[i] = fmt.Sprintf("%s: %s.%s", pk.Name, varName, pk.Name)
	}
	return fmt.Sprintf("%s{%s}", m.PKType(pkgAlias), strings.Join(fields, ", "))
}

// PKValueFromVars constructs a PK key value from local variables named with varPrefix (e.g. "r0_").
func (m Model) PKValueFromVars(varPrefix string, pkgAlias string) string {
	if len(m.PK) == 1 {
		return fmt.Sprintf("%s%s", varPrefix, m.PK[0].Name)
	}
	fields := make([]string, len(m.PK))
	for i, pk := range m.PK {
		fields[i] = fmt.Sprintf("%s: %s%s", pk.Name, varPrefix, pk.Name)
	}
	return fmt.Sprintf("%s{%s}", m.PKType(pkgAlias), strings.Join(fields, ", "))
}

// PKZeroCheckFromVars checks if any PK variable with varPrefix is zero.
func (m Model) PKZeroCheckFromVars(varPrefix string) string {
	checks := make([]string, len(m.PK))
	for i, pk := range m.PK {
		checks[i] = fmt.Sprintf("IsZero(%s%s)", varPrefix, pk.Name)
	}
	return strings.Join(checks, " || ")
}

// UpdatePKPlaceholderStartIdx returns the starting parameter index for the UPDATE WHERE clause.
func (m Model) UpdatePKPlaceholderStartIdx() int {
	return len(m.UpdatableFields()) + 1
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

func scanTarget(varName string, f Field) string {
	if f.IsPointer() || f.Nullable || f.IsTimestamp() {
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

// AllRelationJoins returns the combined LEFT JOIN SQL statements for all model relations,
// incorporating soft-delete filter conditions for related models when present.
func (m Model) AllRelationJoins(parentPrefix string) string {
	var joins []string
	for i, rel := range m.Relations {
		alias := fmt.Sprintf("r%d", i)
		target, ok := m.AllKnownModels[rel.TargetModel]
		if !ok {
			continue
		}

		var joinCond string
		switch rel.Type {
		case RelHasMany:
			joinCond = fmt.Sprintf("LEFT JOIN %s %s ON %s.%s = %s.%s",
				target.Table, alias, alias, rel.ForeignKey, parentPrefix, m.FirstPK().Column)
		case RelBelongsTo:
			joinCond = fmt.Sprintf("LEFT JOIN %s %s ON %s.%s = %s.%s",
				target.Table, alias, alias, target.FirstPK().Column, parentPrefix, rel.ForeignKey)
		}

		if target.HasDeletedAt() {
			joinCond += fmt.Sprintf(" AND %s.%s IS NULL", alias, target.DeletedAtField().Column)
		}

		joins = append(joins, joinCond)
	}
	return strings.Join(joins, "\n\t\t")
}

// HasManyRelations returns the subset of m.Relations with cardinality HasMany.
func (m Model) HasManyRelations() []Relation {
	out := make([]Relation, 0, len(m.Relations))
	for _, r := range m.Relations {
		if r.Type == RelHasMany {
			out = append(out, r)
		}
	}
	return out
}

// BelongsToRelations returns the subset of m.Relations with cardinality BelongsTo.
func (m Model) BelongsToRelations() []Relation {
	out := make([]Relation, 0, len(m.Relations))
	for _, r := range m.Relations {
		if r.Type == RelBelongsTo {
			out = append(out, r)
		}
	}
	return out
}

// UseJoinStrategy reports whether relation preloading should be generated as
// a single LEFT JOIN query.
func (m Model) UseJoinStrategy() bool {
	return len(m.HasManyRelations()) <= 1
}

// JoinRelations returns the relations that participate in the LEFT JOIN query.
func (m Model) JoinRelations() []Relation {
	return m.Relations
}

// SplitHasManyRelations returns the HasMany relations that must be fetched via separate IN queries.
func (m Model) SplitHasManyRelations() []Relation {
	return m.HasManyRelations()
}

func main() {
	inputPkg := flag.String("input", "./example/models", "Path to package containing models")
	outDir := flag.String("out", "./example/queries", "Destination directory for generated code")
	outPkg := flag.String("pkg", "queries", "Package name for generated code")
	schemaOut := flag.String("schema", "", "If set, path to write a generated SQL schema file (e.g. ./schema.sql)")
	dbType := flag.String("dbtype", "postgres", "Target database dialect for -schema: postgres or sqlite3")
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

	// Re-link PK pointers in modelMap entries after all value copies
	for name, m := range modelMap {
		m.PK = nil
		for i := range m.Fields {
			if m.Fields[i].IsPK {
				m.PK = append(m.PK, &m.Fields[i])
			}
		}
		if len(m.PK) == 0 {
			log.Fatalf("query-gen: model %q has no primary key field; models without a primary key are prohibited", name)
		}
		modelMap[name] = m
	}

	if *schemaOut != "" {
		var dialect SchemaDialect
		switch strings.ToLower(*dbType) {
		case "postgres", "postgresql", "pg":
			dialect = DialectPostgres
		case "sqlite3", "sqlite":
			dialect = DialectSQLite
		default:
			log.Fatalf("query-gen: unsupported -dbtype %q: must be postgres or sqlite3", *dbType)
		}

		if err := writeSchemaFile(parsedModels, modelMap, dialect, *schemaOut); err != nil {
			log.Fatalf("query-gen: %v", err)
		}
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("query-gen: creating output directory %q: %v", *outDir, err)
	}

	if err := generateRuntimeFile(*outDir, *outPkg); err != nil {
		log.Fatalf("query-gen: generating runtime file: %v", err)
	}

	pkgAlias := filepath.Base(fullPkgPath)

	for _, model := range parsedModels {
		if len(model.PK) == 0 {
			log.Fatalf("query-gen: model %q has no primary key field; models without a primary key are prohibited", model.Name)
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

// pluralize returns a pluralized table name for a snake_case identifier.
// Words ending in a consonant + "y" (e.g., "category") become "-ies" ("categories").
func pluralize(s string) string {
	if strings.HasSuffix(s, "y") && len(s) > 1 {
		switch s[len(s)-2] {
		case 'a', 'e', 'i', 'o', 'u':
			return s + "s"
		default:
			return strings.TrimSuffix(s, "y") + "ies"
		}
	}
	return s + "s"
}

func isTimeType(t types.Type) bool {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}

	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return false
	}

	if isTimeStruct(st) {
		return true
	}

	// struct { time.Time } (embedded)
	for field := range st.Fields() {
		if field.Embedded() && isTimeType(field.Type()) {
			return true
		}
	}
	return false
}

// isTimeStruct reports whether st matches time.Time's known underlying
// struct shape by field name and type string, avoiding any dependency on
// which type-checking session/importer produced the type.
func isTimeStruct(st *types.Struct) bool {
	if st.NumFields() != 3 {
		return false
	}
	wantNames := [3]string{"wall", "ext", "loc"}
	wantTypes := [3]string{"uint64", "int64", "*time.Location"}
	for i := range 3 {
		f := st.Field(i)
		if f.Name() != wantNames[i] || f.Type().String() != wantTypes[i] {
			return false
		}
	}
	return true
}

func resolveUnderlyingType(expr ast.Expr, fieldType string, typesInfo *types.Info) string {
	if typesInfo == nil {
		return fieldType
	}

	t := typesInfo.TypeOf(expr)
	if t == nil {
		return fieldType
	}

	isPointer := false
	if ptr, ok := t.(*types.Pointer); ok {
		isPointer = true
		t = ptr.Elem()
	}

	if basic, ok := t.Underlying().(*types.Basic); ok {
		if isPointer {
			return "*" + basic.String()
		}
		return basic.String()
	}

	if isTimeType(t) {
		if isPointer {
			return "*time.Time"
		}
		return "time.Time"
	}

	return fieldType
}

// bareTypeName strips pointer indirections and package qualifiers from a type name.
// E.g., "*perms.Date" -> "Date", "models.User" -> "User", "string" -> "string".
func bareTypeName(s string) string {
	s = strings.TrimPrefix(s, "*")
	if idx := strings.LastIndex(s, "."); idx != -1 {
		return s[idx+1:]
	}
	return s
}

func parsePackage(pattern string) ([]Model, string, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedDeps,
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
		tableNames := collectTableNameMethods(pkg.Syntax)
		customDataTypes := collectDataTypeMethods(pkg)

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
					Name:       typeSpec.Name.Name,
					NamePlural: pluralize(typeSpec.Name.Name),
					Table:      pluralize(toSnakeCase(typeSpec.Name.Name)),
				}

				if explicit, ok := tableNames[model.Name]; ok {
					model.Table = explicit
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

					if rel, ok := detectRelation(field, rawTag, model.Name, pkg.TypesInfo); ok {
						model.Relations = append(model.Relations, rel)
						continue
					}

					fieldType := types.ExprString(field.Type)
					parsedField := parseFieldTags(fieldName, fieldType, rawTag)

					if parsedField.IsIgnore {
						continue
					}

					// Resolve underlying primitive Go type using type-checker info
					parsedField.UnderlyingType = resolveUnderlyingType(field.Type, fieldType, pkg.TypesInfo)

					if parsedField.RawSQLType == "" {
						baseType := strings.TrimPrefix(fieldType, "*")
						bareType := bareTypeName(baseType)
						underlyingBase := parsedField.UnderlyingBaseType()
						bareUnderlying := bareTypeName(underlyingBase)

						for _, t := range []string{baseType, underlyingBase, bareType, bareUnderlying} {
							if t != "" {
								if dt, ok := customDataTypes[t]; ok {
									parsedField.RawSQLType = dt
									break
								}
							}
						}
					}
					model.Fields = append(model.Fields, parsedField)
				}

				// Assign PK pointers after all fields are appended
				model.PK = nil
				for i := range model.Fields {
					if model.Fields[i].IsPK {
						model.PK = append(model.PK, &model.Fields[i])
					}
				}

				if len(model.PK) == 0 {
					for i := range model.Fields {
						if strings.EqualFold(model.Fields[i].Name, "ID") {
							model.Fields[i].IsPK = true
							model.PK = append(model.PK, &model.Fields[i])
							break
						}
					}
				}

				if len(model.PK) == 0 {
					log.Fatalf("query-gen: model %q has no primary key field; models without a primary key are prohibited", model.Name)
				}

				result = append(result, model)
				return true
			})
		}
	}

	return result, fullPkgPath, nil
}

// collectDataTypeMethods scans a package and its imported packages for methods with
// the signature `func (r ReceiverType) DataType() string` or
// `func (r ReceiverType) GormDataType() string` that return a single constant
// string literal, returning a map of receiver type name to custom data type.
func collectDataTypeMethods(pkg *packages.Package) map[string]string {
	dataTypes := make(map[string]string)
	visited := make(map[string]bool)

	var collect func(p *packages.Package)
	collect = func(p *packages.Package) {
		if p == nil || visited[p.PkgPath] {
			return
		}
		visited[p.PkgPath] = true

		for _, imp := range p.Imports {
			collect(imp)
		}

		pkgName := p.Name

		for _, file := range p.Syntax {
			for _, decl := range file.Decls {
				funcDecl, ok := decl.(*ast.FuncDecl)
				if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) != 1 {
					continue
				}

				methodName := funcDecl.Name.Name
				if methodName != "DataType" && methodName != "GormDataType" {
					continue
				}

				sig := funcDecl.Type
				if sig.Params != nil && len(sig.Params.List) > 0 {
					continue
				}
				if sig.Results == nil || len(sig.Results.List) != 1 {
					continue
				}
				if resultIdent, ok := sig.Results.List[0].Type.(*ast.Ident); !ok || resultIdent.Name != "string" {
					continue
				}

				receiverType := receiverTypeName(funcDecl.Recv.List[0].Type)
				if receiverType == "" {
					continue
				}

				literal := extractReturnedStringLiteral(funcDecl.Body)
				if literal == "" {
					continue
				}

				// Store both bare name ("Date") and qualified name ("mytypes.Date")
				qualType := pkgName + "." + receiverType

				if methodName == "GormDataType" || dataTypes[receiverType] == "" {
					dataTypes[receiverType] = literal
					dataTypes[qualType] = literal
				}
			}
		}
	}

	collect(pkg)
	return dataTypes
}

// collectTableNameMethods scans a package's syntax trees for methods with
// the signature `func (r ReceiverType) TableName() string` (value or
// pointer receiver) that return a single constant string literal, and
// returns a map of receiver type name to that literal table name.
func collectTableNameMethods(files []*ast.File) map[string]string {
	names := make(map[string]string)

	for _, file := range files {
		for _, decl := range file.Decls {
			funcDecl, ok := decl.(*ast.FuncDecl)
			if !ok || funcDecl.Recv == nil || len(funcDecl.Recv.List) != 1 {
				continue
			}
			if funcDecl.Name.Name != "TableName" {
				continue
			}

			sig := funcDecl.Type
			if sig.Params != nil && len(sig.Params.List) > 0 {
				continue
			}
			if sig.Results == nil || len(sig.Results.List) != 1 {
				continue
			}
			if resultIdent, ok := sig.Results.List[0].Type.(*ast.Ident); !ok || resultIdent.Name != "string" {
				continue
			}

			receiverType := receiverTypeName(funcDecl.Recv.List[0].Type)
			if receiverType == "" {
				continue
			}

			literal := extractReturnedStringLiteral(funcDecl.Body)
			if literal == "" {
				continue
			}

			names[receiverType] = literal
		}
	}

	return names
}

// receiverTypeName extracts the bare type name from a method receiver expression.
func receiverTypeName(expr ast.Expr) string {
	if starExpr, ok := expr.(*ast.StarExpr); ok {
		expr = starExpr.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// extractReturnedStringLiteral looks for a single, unconditional `return "literal"` statement.
func extractReturnedStringLiteral(body *ast.BlockStmt) string {
	if body == nil || len(body.List) != 1 {
		return ""
	}

	retStmt, ok := body.List[0].(*ast.ReturnStmt)
	if !ok || len(retStmt.Results) != 1 {
		return ""
	}

	basicLit, ok := retStmt.Results[0].(*ast.BasicLit)
	if !ok || basicLit.Kind != token.STRING {
		return ""
	}

	unquoted, err := strconv.Unquote(basicLit.Value)
	if err != nil {
		return ""
	}

	return unquoted
}

func detectRelation(field *ast.Field, rawTag string, parentModelName string, typesInfo *types.Info) (Relation, bool) {
	fieldName := field.Names[0].Name
	rel := Relation{FieldName: fieldName}

	structTag := reflect.StructTag(rawTag)
	gormTag := structTag.Get("gorm")

	fieldType := field.Type

	if arrayType, ok := fieldType.(*ast.ArrayType); ok {
		rel.Type = RelHasMany
		fieldType = arrayType.Elt
	} else {
		rel.Type = RelBelongsTo
	}

	if starExpr, ok := fieldType.(*ast.StarExpr); ok {
		rel.IsPointer = true
		fieldType = starExpr.X
	}

	ident, ok := fieldType.(*ast.Ident)
	if !ok || !ident.IsExported() {
		return rel, false
	}

	if typesInfo != nil {
		if t := typesInfo.TypeOf(fieldType); t != nil {
			if _, isStruct := t.Underlying().(*types.Struct); !isStruct {
				return rel, false
			}
		}
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
			case strings.HasPrefix(part, "constraint:"):
				constraintBody := strings.TrimPrefix(part, "constraint:")
				for clause := range strings.SplitSeq(constraintBody, ",") {
					clause = strings.TrimSpace(clause)
					switch {
					case strings.HasPrefix(strings.ToUpper(clause), "ONDELETE:"):
						rel.OnDelete = strings.ToUpper(strings.TrimSpace(clause[len("OnDelete:"):]))
					case strings.HasPrefix(strings.ToUpper(clause), "ONUPDATE:"):
						rel.OnUpdate = strings.ToUpper(strings.TrimSpace(clause[len("OnUpdate:"):]))
					}
				}
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

// parseFieldTags parses a struct field's name, type, and raw tag string into a
// Field description, interpreting `gorm:"..."` tag syntax per GORM's tag
// specification (https://gorm.io/docs/models.html#Fields-Tags).
//
// fieldName is the Go struct field's identifier (e.g. "UserID"), fieldType is
// its textual Go type (e.g. "*string"), and rawTag is the field's full raw
// struct tag string (e.g. `gorm:"column:user_id;not null"`), as obtained from
// reflect.StructField.Tag.
//
// The returned Field has sensible defaults derived from fieldName and
// fieldType even when rawTag contains no "gorm" key, or is empty.
func parseFieldTags(fieldName, fieldType, rawTag string) Field {
	// Seed the result with defaults inferred purely from the field's Go-level
	// name and type, before any tag parsing occurs. These are overridden below
	// as relevant gorm tag options are discovered.
	f := Field{
		Name:       fieldName,
		Type:       fieldType,
		Column:     toSnakeCase(fieldName),             // default column name, e.g. "UserID" -> "user_id"
		IsPK:       strings.EqualFold(fieldName, "ID"), // GORM convention: a field literally named "ID" (any case) is the primary key
		Permission: PermReadWrite,                      // fields are read/write unless a tag says otherwise
	}

	// A Go pointer type (e.g. "*string") signals a nullable column by GORM
	// convention, independent of any explicit "null"/"not null" tag.
	f.Nullable = strings.HasPrefix(fieldType, "*")

	// No struct tag at all: nothing further to parse, return the type-derived defaults.
	if rawTag == "" {
		return f
	}

	// Wrap the raw tag string in reflect.StructTag so we can extract just the
	// "gorm" key's value (GORM tags are namespaced under gorm:"...").
	structTag := reflect.StructTag(rawTag)
	gormTag := structTag.Get("gorm")
	if gormTag == "" {
		// The field has other tags (e.g. json:"...") but no gorm tag; nothing
		// GORM-specific to apply.
		return f
	}

	// --- First pass: column name only -----------------------------------
	// GORM tag options are semicolon-separated. This first pass scans only
	// for "column:" so that f.Column is resolved before anything else,
	// independent of where "column:" appears relative to other options.
	// It stops at the first match via break, so only the first "column:"
	// segment found takes effect if there are (invalidly) multiple.
	for part := range strings.SplitSeq(gormTag, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), "column:") {
			// NOTE: uses the original-case `part`, not `lower`, so the actual
			// column name preserves the case the user wrote after "column:".
			f.Column = part[len("column:"):]
			break
		}
	}

	// Local accumulators for read/create/update permission bits, combined
	// into f.Permission after the main loop. All default to true (readable
	// and writable) and are narrowed by "->" / "<-" style tags below.
	canRead := true
	canCreate := true
	canUpdate := true
	// Track whether a "->" (read) or "<-" (write) style tag was seen at all,
	// distinguishing "explicitly set to true" from "never mentioned".
	hasReadTag := false
	hasWriteTag := false

	// --- Second pass: every other option ---------------------------------
	for part := range strings.SplitSeq(gormTag, ";") {
		part = strings.TrimSpace(part)
		lower := strings.ToLower(part)

		switch {
		// "-" ignores the field entirely (read, write, and migration); "-:all"
		// is the explicit long form of the same thing. Either way, return
		// immediately: no other tag option can matter once the field is ignored.
		case part == "-" || lower == "-:all":
			f.IsIgnore = true
			return f

		// "-:migration" excludes the field from auto-migration only; it still
		// participates in normal read/write. No field on Field models this
		// distinction here, so the case is present but intentionally a no-op.
		case lower == "-:migration":

		case lower == "primarykey" || lower == "primary_key":
			f.IsPK = true

		case lower == "unique":
			f.IsUnique = true

		case lower == "index":
			f.HasIndex = true

		// "index:name" — a named index. Checked after the bare "index" case;
		// since lower == "index" won't match this HasPrefix branch (no colon),
		// order between the two doesn't actually matter here.
		case strings.HasPrefix(lower, "index:"):
			f.HasIndex = true
			f.IndexName = part[len("index:"):] // preserves original case of the name

		case lower == "uniqueindex" || lower == "unique_index":
			f.HasUniqueIndex = true

		case strings.HasPrefix(lower, "uniqueindex:"):
			f.HasUniqueIndex = true
			f.UniqueIndexName = part[len("uniqueindex:"):]

		// GORM accepts both "uniqueIndex:" and "unique_index:" spellings for a
		// named unique index; handled as two separate prefix cases since the
		// prefix lengths differ.
		case strings.HasPrefix(lower, "unique_index:"):
			f.HasUniqueIndex = true
			f.UniqueIndexName = part[len("unique_index:"):]

		case strings.HasPrefix(lower, "default:"):
			f.HasDefault = true
			f.DefaultVal = part[len("default:"):]

		case strings.HasPrefix(lower, "type:"):
			f.RawSQLType = part[len("type:"):]

		// "datatype:" is an alternate/legacy spelling for an explicit SQL
		// type. Only applied if "type:" hasn't already set RawSQLType, so
		// "type:" takes priority when both are present.
		case strings.HasPrefix(lower, "datatype:"):
			if f.RawSQLType == "" {
				f.RawSQLType = part[len("datatype:"):]
			}

		// "size:N" — malformed/non-numeric values are silently ignored
		// (err != nil skips the assignment) rather than causing a parse failure.
		case strings.HasPrefix(lower, "size:"):
			if n, err := strconv.Atoi(part[len("size:"):]); err == nil {
				f.Size = n
			}

		case strings.HasPrefix(lower, "precision:"):
			if n, err := strconv.Atoi(part[len("precision:"):]); err == nil {
				f.Precision = n
			}

		case strings.HasPrefix(lower, "scale:"):
			if n, err := strconv.Atoi(part[len("scale:"):]); err == nil {
				f.Scale = n
			}

		// "check:constraint" or "check:name,constraint". Uses the
		// case-preserving `part` (not `lower`) since SQL check expressions
		// are case-sensitive. If a comma is present, everything before the
		// first comma is treated as an optional constraint name and
		// discarded; everything after is the constraint expression itself.
		// Index is used (rather than LastIndex) so a constraint expression
		// that itself contains a comma doesn't get truncated.
		case strings.HasPrefix(part, "check:"):
			raw := part[len("check:"):]
			if _, after, ok := strings.Cut(raw, ","); ok {
				f.CheckConstraint = strings.TrimSpace(after)
			} else {
				f.CheckConstraint = strings.TrimSpace(raw)
			}

		case lower == "not null" || lower == "notnull":
			f.Nullable = false

		case lower == "null":
			f.Nullable = true

		// "->" family: read permission tags. Bare "->", "->:true", "->:rw",
		// and "->:r" are all treated as "readable" (GORM's finer-grained
		// distinctions between these forms aren't modeled separately here).
		case lower == "->" || lower == "->:true" || lower == "->:rw" || lower == "->:r":
			canRead = true
			hasReadTag = true

		case lower == "->:false":
			canRead = false
			hasReadTag = true

		// "<-" family: write permission tags. Bare "<-" and "<-:true" both
		// mean fully writable (create + update).
		case lower == "<-" || lower == "<-:true" || lower == "<-:rw":
			canCreate = true
			canUpdate = true
			hasWriteTag = true

		case lower == "<-:false":
			canCreate = false
			canUpdate = false
			hasWriteTag = true

		case lower == "<-:create":
			canCreate = true
			canUpdate = false
			hasWriteTag = true

		case lower == "<-:update":
			canCreate = false
			canUpdate = true
			hasWriteTag = true
		}
	}

	// If a read-permission tag was given but no write-permission tag was
	// given, and the field is still marked readable, GORM's semantics treat
	// this as "read-only unless writability is separately declared" — so
	// creation/update are turned off. Without this adjustment, a bare "->"
	// tag (meaning "readable") would otherwise leave canCreate/canUpdate at
	// their true defaults and incorrectly produce PermReadWrite.
	if hasReadTag && !hasWriteTag && canRead {
		canCreate = false
		canUpdate = false
	}

	// Collapse the three canRead/canCreate/canUpdate booleans into the
	// Field's single Permission value. The first five cases cover the
	// "clean" combinations with dedicated named constants; anything else
	// (i.e. the remaining three of the eight possible boolean combinations,
	// including "all false") falls through to the default branch, which
	// OR-bits together whichever of PermReadOnly/PermCreateOnly/PermUpdateOnly
	// apply. Note this presumes Permission is a bitmask type where those
	// single-capability constants can be safely combined with |.
	switch {
	case canRead && canCreate && canUpdate:
		f.Permission = PermReadWrite
	case canRead && !canCreate && !canUpdate:
		f.Permission = PermReadOnly
	case !canRead && canCreate && canUpdate:
		f.Permission = PermWriteOnly
	case !canRead && canCreate && !canUpdate:
		f.Permission = PermCreateOnly
	case !canRead && !canCreate && canUpdate:
		f.Permission = PermUpdateOnly
	default:
		var perm Permission
		if canRead {
			perm |= PermReadOnly
		}
		if canCreate {
			perm |= PermCreateOnly
		}
		if canUpdate {
			perm |= PermUpdateOnly
		}
		f.Permission = perm
	}

	// Primary keys are never nullable.
	if f.IsPK {
		f.Nullable = false
	}

	return f
}

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

// --- Runtime Helper Template ---

const runtimeTemplate = `// Code generated by query-gen. DO NOT EDIT.
package {{.Package}}

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// inBatchSize sets the maximum number of bind parameters per IN clause batch.
// 999 is chosen to conform to SQLite3's default maximum limit (SQLITE_MAX_VARIABLE_NUMBER).
const inBatchSize = 999

// batchIn splits ids into chunks of at most inBatchSize and executes fetch for each batch,
// concatenating results to avoid exceeding database SQL parameter limits.
func batchIn[T, K any](ctx context.Context, db DBTX, ids []K, fetch func(ctx context.Context, db DBTX, batch []K) ([]T, error)) ([]T, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) <= inBatchSize {
		return fetch(ctx, db, ids)
	}

	results := make([]T, 0, len(ids))
	for i := 0; i < len(ids); i += inBatchSize {
		end := min(i+inBatchSize, len(ids))
		batch, err := fetch(ctx, db, ids[i:end])
		if err != nil {
			return nil, fmt.Errorf("batchIn: batch [%d:%d]: %w", i, end, err)
		}
		results = append(results, batch...)
	}
	return results, nil
}

// QueryOptions provides optional filtering, ordering, grouping, having, pagination, association preloading, and soft-delete controls for queries.
type QueryOptions struct {
	Where               string
	Args                []any
	Having              string
	OrderBy             string
	GroupBy             string
	Limit               int
	Offset              int
	PreloadAssociations bool
	IncludeDeleted      bool
	HardDelete          bool
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

// ILIKE performs a case-insensitive pattern match using ILIKE (or LOWER for cross-dialect compatibility).
func ILIKE(column, value string) QueryOption {
	return func(o *QueryOptions) {
		if value == "" {
			return
		}

		o.Args = append(o.Args, "%"+strings.ToLower(value)+"%")
		clause := fmt.Sprintf("LOWER(%s) LIKE $%d", column, len(o.Args))

		if o.Where != "" {
			o.Where += " AND " + clause
		} else {
			o.Where = clause
		}
	}
}

// staticPlaceholders pre-computes "$1" through "$1000" to eliminate string formatting
// allocations during query option construction and parameter binding.
var staticPlaceholders = func() [1001]string {
	var p [1001]string
	for i := 1; i <= 1000; i++ {
		p[i] = fmt.Sprintf("$%d", i)
	}
	return p
}()

// getPlaceholder returns a cached placeholder string for index <= 1000,
// falling back to fmt.Sprintf for higher indices.
func getPlaceholder(idx int) string {
	if idx >= 1 && idx <= 1000 {
		return staticPlaceholders[idx]
	}
	return fmt.Sprintf("$%d", idx)
}

// In filters a column matching any of the provided values.
// It pre-allocates buffer memory and uses cached placeholder strings to minimize allocations.
func In(column string, values ...any) QueryOption {
	return func(o *QueryOptions) {
		n := len(values)
		if n == 0 {
			return
		}

		// Pre-allocate o.Args slice capacity in a single grow step
		if cap(o.Args)-len(o.Args) < n {
			newArgs := make([]any, len(o.Args), len(o.Args)+n)
			copy(newArgs, o.Args)
			o.Args = newArgs
		}

		// Pre-allocate string builder buffer capacity
		var sb strings.Builder
		sb.Grow(len(column) + 6 + (n * 6))
		sb.WriteString(column)
		sb.WriteString(" IN (")

		for i, v := range values {
			if i > 0 {
				sb.WriteString(", ")
			}
			o.Args = append(o.Args, v)
			sb.WriteString(getPlaceholder(len(o.Args)))
		}
		sb.WriteByte(')')

		clause := sb.String()
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

// Preload enables or disables preloading associated model relations.
func Preload(preload bool) QueryOption {
	return func(o *QueryOptions) {
		o.PreloadAssociations = preload
	}
}

// IncludeDeleted includes soft-deleted records (where deleted_at IS NOT NULL) in query results.
func IncludeDeleted() QueryOption {
	return func(o *QueryOptions) {
		o.IncludeDeleted = true
	}
}

// HardDelete forces permanent SQL deletion on models supporting soft deletes.
func HardDelete() QueryOption {
	return func(o *QueryOptions) {
		o.HardDelete = true
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

func applyQueryOptions(defaultPK string, deletedAtCol string, opts ...QueryOption) (string, []any, QueryOptions) {
	cfg := parseQueryOptions(opts...)

	if deletedAtCol != "" && !cfg.IncludeDeleted {
		clause := deletedAtCol + " IS NULL"
		if cfg.Where != "" {
			cfg.Where = clause + " AND " + cfg.Where
		} else {
			cfg.Where = clause
		}
	}

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

	// ORDER BY, LIMIT, and OFFSET only apply to row-fetching queries (when defaultPK != ""),
	// avoiding invalid ORDER BY clauses in aggregate COUNT(*) queries.
	if defaultPK != "" {
		if cfg.OrderBy != "" {
			sb.WriteString(" ORDER BY ")
			sb.WriteString(cfg.OrderBy)
		} else {
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
// Page is constrained to (1, 10) if page < 1 or pageSize < 1.
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

// nullableScanner adapts a *T destination to the sql.Scanner interface,
// handling NULL source values by zeroing dst, and falling back to
// sql.Scanner or convertAssign for types that don't directly assert to T.
type nullableScanner[T any] struct {
	dst *T // Destination field. Not owned; caller retains ownership.
}

// Scan implements sql.Scanner.
func (n nullableScanner[T]) Scan(src any) error {
	if src == nil {
		var zero T
		*n.dst = zero
		return nil
	}

	if v, ok := src.(T); ok {
		*n.dst = v
		return nil
	}

	if err := convertAssign(n.dst, src); err == nil {
		return nil
	}

	if scanner, ok := any(n.dst).(sql.Scanner); ok {
		return scanner.Scan(src)
	}

	// n.dst is like **string, so vDst is *string
	vDst := reflect.ValueOf(n.dst).Elem() 
	if vDst.Kind() == reflect.Pointer {
		if vDst.IsNil() {
			// Automatically allocates a new type: e.g new(string)
			vDst.Set(reflect.New(vDst.Type().Elem()))
		}
		// Passes the inner *string to convertAssign
		return convertAssign(vDst.Interface(), src)
	}
	return fmt.Errorf("scanNullable: cannot scan %T into %T", src, n.dst)
}

// scanNullable returns a value implementing sql.Scanner for dst.
func scanNullable[T any](dst *T) nullableScanner[T] {
	return nullableScanner[T]{dst: dst}
}

// parseTimeString parses SQL and ISO 8601 timestamp strings.
func parseTimeString(s string) (time.Time, error) {
	formats := []string{
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02",
		"Mon Jan 2 15:04:05 MST 2006",
		"Mon Jan  2 15:04:05 MST 2006",
	}

	for _, fmtStr := range formats {
		if t, err := time.Parse(fmtStr, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("parseTimeString: failed to parse time string %q", s)
}

func convertAssign(dst any, src any) error {
	if src == nil {
		return nil
	}

	switch p := dst.(type) {
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
			t, err := parseTimeString(s)
			if err != nil {
				return err
			}
			*p = t
			return nil
		case []byte:
			t, err := parseTimeString(string(s))
			if err != nil {
				return err
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
	case *time.Duration:
		if i, ok := toInt64(src); ok {
			*p = time.Duration(i)
			return nil
		}
		switch s := src.(type) {
		case []byte:
			i, err := strconv.ParseInt(string(s), 10, 64)
			if err != nil {
				return fmt.Errorf("convertAssign: failed to parse duration bytes %q: %w", s, err)
			}
			*p = time.Duration(i)
			return nil
		case string:
			i, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return fmt.Errorf("convertAssign: failed to parse duration string %q: %w", s, err)
			}
			*p = time.Duration(i)
			return nil
		}
	}

	vDst := reflect.ValueOf(dst)
	if vDst.Kind() != reflect.Pointer || vDst.IsNil() {
		return fmt.Errorf("convertAssign: dst must be a non-nil pointer, got %T", dst)
	}

	vElem := vDst.Elem()
	vSrc := reflect.ValueOf(src)

	if vSrc.Type().ConvertibleTo(vElem.Type()) {
		vElem.Set(vSrc.Convert(vElem.Type()))
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

// seenKey uniquely identifies a (parent, child) primary key pair.
type seenKey[P comparable, C comparable] struct {
	parent P
	child  C
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
		{{range .WritableFields -}}
		{col: "{{.Column}}", val: m.{{.Name}}, omitIfZero: {{or .HasDefault (and .IsTimestamp (not .Nullable))}}},
		{{end -}}
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

	if err := db.QueryRowContext(ctx, query, args...).
		Scan({{.AllScanArgs "m"}}); err != nil {
		return fmt.Errorf("insert{{.Name}}: %w", err)
	}
	return nil
}

// ============ Bulk insert ========================

// Insert{{.NamePlural}} inserts multiple {{.Name}} records into {{.Table}} in efficient parameter-bounded batches.
// Automatically populates database-generated values (e.g., auto-increment primary keys, default values) back into input structs via RETURNING.
func Insert{{.NamePlural}}(ctx context.Context, db DBTX, models []*{{.ModelPkgAlias}}.{{.Name}}) error {
	if db == nil {
		return errors.New("insert{{.NamePlural}}: db is nil")
	}
	if len(models) == 0 {
		return nil
	}
		
	const batchSize = {{.BulkInsertBatchSize}}
	for i := 0; i < len(models); i += batchSize {
		end := min((i + batchSize), len(models))
		batch := models[i:end]
		if err := insert{{.NamePlural}}Batch(ctx, db, batch); err != nil {
			return fmt.Errorf("insert{{.NamePlural}}: batch [%d:%d]: %w", i, end, err)
		}
	}
	return nil
}

func insert{{.NamePlural}}Batch(ctx context.Context, db DBTX, batch []*{{.ModelPkgAlias}}.{{.Name}}) error {
	if len(batch) == 0 {
		return nil
	}

	{{$writable := .WritableFields -}}
	{{$numCols := len $writable -}}
	{{if eq $numCols 0 -}}
	for i, m := range batch {
		if m == nil {
			return fmt.Errorf("insert{{$.NamePlural}}Batch: model at index %d is nil", i)
		}
		const query = "INSERT INTO {{$.Table}} DEFAULT VALUES RETURNING {{$.AllColumns ""}}"
		if err := db.QueryRowContext(ctx, query).Scan({{$.AllScanArgs "m"}}); err != nil {
			return fmt.Errorf("insert{{$.NamePlural}}Batch row %d: %w", i, err)
		}
	}
	return nil
	{{else -}}
	args := make([]any, 0, len(batch)*{{$numCols}})
	var sb strings.Builder
	sb.Grow(128 + len(batch)*{{$numCols}}*8)
	sb.WriteString("INSERT INTO {{.Table}} ({{.InsertColumns}}) VALUES ")

	paramIdx := 1
	for i, m := range batch {
		if m == nil {
			return fmt.Errorf("model at index %d is nil", i)
		}
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('(')
		{{range $j, $f := $writable -}}
		{{if gt $j 0}}sb.WriteString(", "){{end}}
		sb.WriteString(getPlaceholder(paramIdx))
		paramIdx++
		args = append(args, m.{{$f.Name}})
		{{end -}}
		sb.WriteByte(')')
	}

	sb.WriteString(" RETURNING {{.AllColumns ""}}")
	rows, err := db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return fmt.Errorf("executing bulk insert: %w", err)
	}
	defer rows.Close()

	idx := 0
	for rows.Next() {
		if idx >= len(batch) {
			return errors.New("unexpected extra row returned from insert")
		}
		m := batch[idx]
		if err := rows.Scan({{.AllScanArgs "m"}}); err != nil {
			return fmt.Errorf("scanning row %d: %w", idx, err)
		}
		idx++
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating bulk insert rows: %w", err)
	}

	if idx < len(batch) {
		return fmt.Errorf("expected %d inserted rows, got %d", len(batch), idx)
	}

	return nil
	{{end -}}
}


// Get{{.Name}}ByID retrieves a single {{.Name}} record from {{.Table}} by its primary key.
func Get{{.Name}}ByID(ctx context.Context, db DBTX, {{.PKParams .ModelPkgAlias}}, opts ...QueryOption) (*{{.ModelPkgAlias}}.{{.Name}}, error) {
	if db == nil {
		return nil, errors.New("get{{.Name}}ByID: db is nil")
	}
	{{- if or .Relations .HasDeletedAt}}

	cfg := parseQueryOptions(opts...)
	{{- end}}
	{{- if .Relations}}

	if cfg.PreloadAssociations {
		return get{{.Name}}ByIDWithRelations(ctx, db, {{.PKArgs ""}}, cfg)
	}
	{{- end}}
	{{- if .HasDeletedAt}}

	query := ` + "`" + `
		SELECT {{.AllColumns ""}}
		FROM {{.Table}}
		WHERE {{.PKWhereClause "" 1}}
	` + "`" + `
	if !cfg.IncludeDeleted {
		query += " AND {{.DeletedAtField.Column}} IS NULL"
	}
	{{- else}}

	const query = ` + "`" + `
		SELECT {{.AllColumns ""}}
		FROM {{.Table}}
		WHERE {{.PKWhereClause "" 1}}
	` + "`" + `
	{{- end}}

	row := db.QueryRowContext(ctx, query, {{.PKArgs ""}})
	var m {{.ModelPkgAlias}}.{{.Name}}
	if err := row.Scan({{.AllScanArgs "m"}}); err != nil {
		return nil, fmt.Errorf("get{{.Name}}ByID(%v): %w", {{if eq (len .PK) 1}}id{{else}}fmt.Sprint({{.PKArgs ""}}){{end}}, err)
	}

	return &m, nil
}

// Exists{{.Name}}ByID reports whether a {{.Name}} record with the given primary key exists.
func Exists{{.Name}}ByID(ctx context.Context, db DBTX, {{.PKParams .ModelPkgAlias}}, opts ...QueryOption) (bool, error) {
	if db == nil {
		return false, errors.New("exists{{.Name}}ByID: db is nil")
	}
	{{- if .HasDeletedAt}}

	cfg := parseQueryOptions(opts...)
	query := ` + "`" + `SELECT EXISTS(SELECT 1 FROM {{.Table}} WHERE {{.PKWhereClause "" 1}}` + "`" + `
	if !cfg.IncludeDeleted {
		query += " AND {{.DeletedAtField.Column}} IS NULL"
	}
	query += ")"
	{{- else}}

	const query = ` + "`" + `SELECT EXISTS(SELECT 1 FROM {{.Table}} WHERE {{.PKWhereClause "" 1}})` + "`" + `
	{{- end}}

	var exists bool
	if err := db.QueryRowContext(ctx, query, {{.PKArgs ""}}).Scan(&exists); err != nil {
		return false, fmt.Errorf("exists{{.Name}}ByID(%v): %w", {{if eq (len .PK) 1}}id{{else}}fmt.Sprint({{.PKArgs ""}}){{end}}, err)
	}
	return exists, nil
}

// Count{{.NamePlural}} returns total records matching optional filter criteria.
func Count{{.NamePlural}}(ctx context.Context, db DBTX, opts ...QueryOption) (int64, error) {
	if db == nil {
		return 0, errors.New("count{{.NamePlural}}: db is nil")
	}

	cfg := parseQueryOptions(opts...)

	var whereClause string
	if cfg.Where != "" {
		whereClause = " WHERE " + cfg.Where
	}
	{{- if .HasDeletedAt}}

	if !cfg.IncludeDeleted {
		if whereClause != "" {
			whereClause += " AND {{.DeletedAtField.Column}} IS NULL"
		} else {
			whereClause = " WHERE {{.DeletedAtField.Column}} IS NULL"
		}
	}
	{{- end}}

	query := "SELECT COUNT(*) FROM {{.Table}}" + whereClause

	var count int64
	if err := db.QueryRowContext(ctx, query, cfg.Args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count{{.NamePlural}}: %w", err)
	}
	return count, nil
}

// FetchAll{{.NamePlural}} retrieves a filtered/paginated slice of {{.Name}} records.
func FetchAll{{.NamePlural}}(ctx context.Context, db DBTX, opts ...QueryOption) ([]*{{.ModelPkgAlias}}.{{.Name}}, error) {
	if db == nil {
		return nil, errors.New("fetchAll{{.NamePlural}}: db is nil")
	}
	{{- if .Relations}}

	clause, args, cfg := applyQueryOptions("{{.PKColumns ""}}", {{if .HasDeletedAt}}"{{.DeletedAtField.Column}}"{{else}}""{{end}}, opts...)
	if cfg.PreloadAssociations {
		return fetchAll{{.NamePlural}}WithRelations(ctx, db, clause, args, cfg)
	}
	{{- else}}

	clause, args, _ := applyQueryOptions("{{.PKColumns ""}}", {{if .HasDeletedAt}}"{{.DeletedAtField.Column}}"{{else}}""{{end}}, opts...)
	{{- end}}

	query := "SELECT {{.AllColumns ""}} FROM {{.Table}}" + clause

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fetchAll{{.NamePlural}}: %w", err)
	}
	defer rows.Close()

	items := make([]*{{.ModelPkgAlias}}.{{.Name}}, 0, 16)
	for rows.Next() {
		var m {{.ModelPkgAlias}}.{{.Name}}
		if err := rows.Scan({{.AllScanArgs "m"}}); err != nil {
			return nil, fmt.Errorf("fetchAll{{.NamePlural}}: scanning row: %w", err)
		}
		items = append(items, &m)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetchAll{{.NamePlural}}: iterating rows: %w", err)
	}

	return items, nil
}

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
		WHERE {{.PKWhereClause "" .UpdatePKPlaceholderStartIdx}}
	` + "`" + `

	res, err := db.ExecContext(ctx, query, {{.UpdatableScanArgs "m"}}, {{.PKArgs "m."}})
	if err != nil {
		return fmt.Errorf("update{{.Name}}(%v): %w", fmt.Sprint({{.PKArgs "m."}}), err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("detect rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("update{{.Name}}(%v): %w", fmt.Sprint({{.PKArgs "m."}}), sql.ErrNoRows)
	}
	return nil
}

// Delete{{.Name}} deletes the {{.Name}} record identified by primary key from {{.Table}}.
func Delete{{.Name}}(ctx context.Context, db DBTX, {{.PKParams .ModelPkgAlias}}, opts ...QueryOption) error {
	if db == nil {
		return errors.New("delete{{.Name}}: db is nil")
	}
	{{- if .HasDeletedAt}}

	cfg := parseQueryOptions(opts...)
	if !cfg.HardDelete {
		const query = ` + "`" + `UPDATE {{.Table}} SET {{.DeletedAtField.Column}} = CURRENT_TIMESTAMP WHERE {{.PKWhereClause "" 1}} AND {{.DeletedAtField.Column}} IS NULL` + "`" + `
		res, err := db.ExecContext(ctx, query, {{.PKArgs ""}})
		if err != nil {
			return fmt.Errorf("delete{{.Name}}(%v): %w", {{if eq (len .PK) 1}}id{{else}}fmt.Sprint({{.PKArgs ""}}){{end}}, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("detect rows affected: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("delete{{.Name}}(%v): %w", {{if eq (len .PK) 1}}id{{else}}fmt.Sprint({{.PKArgs ""}}){{end}}, sql.ErrNoRows)
		}
		return nil
	}
	{{- end}}

	const query = ` + "`" + `DELETE FROM {{.Table}} WHERE {{.PKWhereClause "" 1}}` + "`" + `

	res, err := db.ExecContext(ctx, query, {{.PKArgs ""}})
	if err != nil {
		return fmt.Errorf("delete{{.Name}}(%v): %w", {{if eq (len .PK) 1}}id{{else}}fmt.Sprint({{.PKArgs ""}}){{end}}, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("detect rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete{{.Name}}(%v): %w", {{if eq (len .PK) 1}}id{{else}}fmt.Sprint({{.PKArgs ""}}){{end}}, sql.ErrNoRows)
	}
	return nil
}

// Delete{{.NamePlural}} deletes records from {{.Table}} matching the provided query options and returns the number of affected rows.
func Delete{{.NamePlural}}(ctx context.Context, db DBTX, opts ...QueryOption) (int64, error) {
	if db == nil {
		return 0, errors.New("delete{{.NamePlural}}: db is nil")
	}

	cfg := parseQueryOptions(opts...)
	if cfg.Where == "" {
		return 0, errors.New("delete{{.NamePlural}}: query options/where clause required to prevent accidental bulk deletion")
	}
	{{- if .HasDeletedAt}}

	clause, args, cfg := applyQueryOptions("", "{{.DeletedAtField.Column}}", opts...)
	{{- else}}

	clause, args, _ := applyQueryOptions("", "", opts...)
	{{- end}}

	var query string
	{{- if .HasDeletedAt}}
	if !cfg.HardDelete {
		query = "UPDATE {{.Table}} SET {{.DeletedAtField.Column}} = CURRENT_TIMESTAMP" + clause
	} else {
		query = "DELETE FROM {{.Table}}" + clause
	}
	{{- else}}
	query = "DELETE FROM {{.Table}}" + clause
	{{- end}}

	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("delete{{.NamePlural}}: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete{{.NamePlural}}: detecting rows affected: %w", err)
	}

	return n, nil
}

// ================= FETCHING RELATIONS ===============================
{{- if .Relations}}
{{- if .UseJoinStrategy}}
func get{{.Name}}ByIDWithRelations(ctx context.Context, db DBTX, {{.PKParams .ModelPkgAlias}}, cfg QueryOptions) (*{{.ModelPkgAlias}}.{{.Name}}, error) {
	{{- if .HasDeletedAt}}
	query := ` + "`" + `
		SELECT 
			{{.AllColumns "p"}}{{.AllRelationColumns}}
		FROM {{.Table}} p
		{{.AllRelationJoins "p"}}
		WHERE {{.PKWhereClause "p" 1}}
	` + "`" + `
	if !cfg.IncludeDeleted {
		query += " AND p.{{.DeletedAtField.Column}} IS NULL"
	}
	{{- else}}
	const query = ` + "`" + `
		SELECT 
			{{.AllColumns "p"}}{{.AllRelationColumns}}
		FROM {{.Table}} p
		{{.AllRelationJoins "p"}}
		WHERE {{.PKWhereClause "p" 1}}
	` + "`" + `
	{{- end}}

	rows, err := db.QueryContext(ctx, query, {{.PKArgs ""}})
	if err != nil {
		return nil, fmt.Errorf("get{{.Name}}ByIDWithRelations(%v): %w", {{if eq (len .PK) 1}}id{{else}}fmt.Sprint({{.PKArgs ""}}){{end}}, err)
	}
	defer rows.Close()

	var parent *{{.ModelPkgAlias}}.{{.Name}}
	var p {{.ModelPkgAlias}}.{{.Name}}

	{{range $idx, $rel := .Relations -}}
	{{$target := index $.AllKnownModels $rel.TargetModel -}}
	seen_{{$rel.FieldName}} := make(map[{{$target.PKType $.ModelPkgAlias}}]struct{}, 4)
	var c{{$idx}} {{$.ModelPkgAlias}}.{{$target.Name}}
	{{end -}}

	scanArgs := []any{
		{{.AllScanArgs "p"}},
		{{range $idx, $rel := .Relations -}}
		{{$target := index $.AllKnownModels $rel.TargetModel -}}
		{{range $target.SelectableFields -}}
		scanNullable(&c{{$idx}}.{{.Name}}),
		{{end -}}
		{{end -}}
	}

	for rows.Next() {
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, fmt.Errorf("get{{.Name}}ByIDWithRelations(%v): scanning row: %w", {{if eq (len .PK) 1}}id{{else}}fmt.Sprint({{.PKArgs ""}}){{end}}, err)
		}

		if parent == nil {
			parent = &p
		}

		{{range $idx, $rel := .Relations -}}
		{{$target := index $.AllKnownModels $rel.TargetModel -}}
		if !({{$target.PKZeroCheck (print "c" $idx)}}) {
			rPk{{$idx}} := {{$target.PKValue (print "c" $idx) $.ModelPkgAlias}}
			if _, ok := seen_{{$rel.FieldName}}[rPk{{$idx}}]; !ok {
				seen_{{$rel.FieldName}}[rPk{{$idx}}] = struct{}{}
				child := c{{$idx}}
				{{if eq $rel.Type "HasMany" -}}
				{{if $rel.IsPointer -}}
				parent.{{$rel.FieldName}} = append(parent.{{$rel.FieldName}}, &child)
				{{else -}}
				parent.{{$rel.FieldName}} = append(parent.{{$rel.FieldName}}, child)
				{{end -}}
				{{else if eq $rel.Type "BelongsTo" -}}
				{{if $rel.IsPointer -}}
				parent.{{$rel.FieldName}} = &child
				{{else -}}
				parent.{{$rel.FieldName}} = child
				{{end -}}
				{{end -}}
			}
		}
		{{end -}}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get{{.Name}}ByIDWithRelations(%v): iterating rows: %w", {{if eq (len .PK) 1}}id{{else}}fmt.Sprint({{.PKArgs ""}}){{end}}, err)
	}
	if parent == nil {
		return nil, fmt.Errorf("get{{.Name}}ByIDWithRelations(%v): %w", {{if eq (len .PK) 1}}id{{else}}fmt.Sprint({{.PKArgs ""}}){{end}}, sql.ErrNoRows)
	}

	return parent, nil
}

func fetchAll{{.NamePlural}}WithRelations(ctx context.Context, db DBTX, clause string, args []any, cfg QueryOptions) ([]*{{.ModelPkgAlias}}.{{.Name}}, error) {
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
		ORDER BY {{.PKColumns "p"}} ASC
	` + "`" + `

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fetchAll{{.NamePlural}}WithRelations: %w", err)
	}
	defer rows.Close()

	items := make([]*{{.ModelPkgAlias}}.{{.Name}}, 0, 16)
	itemsMap := make(map[{{.PKType .ModelPkgAlias}}]*{{.ModelPkgAlias}}.{{.Name}}, 16)

	{{range $idx, $rel := .Relations -}}
	{{$target := index $.AllKnownModels $rel.TargetModel -}}
	seen_{{$rel.FieldName}} := make(map[seenKey[{{$.PKType $.ModelPkgAlias}}, {{$target.PKType $.ModelPkgAlias}}]]struct{}, 64)
	{{end -}}

	for rows.Next() {
		var p {{.ModelPkgAlias}}.{{.Name}}
		{{range $idx, $rel := .Relations -}}
		{{$target := index $.AllKnownModels $rel.TargetModel -}}
		var c{{$idx}} {{$.ModelPkgAlias}}.{{$target.Name}}
		{{end -}}

		scanArgs := []any{
			{{.AllScanArgs "p"}},
			{{range $idx, $rel := .Relations -}}
			{{$target := index $.AllKnownModels $rel.TargetModel -}}
			{{range $target.SelectableFields -}}
			scanNullable(&c{{$idx}}.{{.Name}}),
			{{end -}}
			{{end -}}
		}

		if err := rows.Scan(scanArgs...); err != nil {
			return nil, fmt.Errorf("fetchAll{{.NamePlural}}WithRelations: scanning row: %w", err)
		}

		pPK := {{.PKValue "p" .ModelPkgAlias}}
		parent, exists := itemsMap[pPK]
		if !exists {
			parent = &p
			itemsMap[pPK] = parent
			items = append(items, parent)
		}

		{{range $idx, $rel := .Relations -}}
		{{$target := index $.AllKnownModels $rel.TargetModel -}}
		if !({{$target.PKZeroCheck (print "c" $idx)}}) {
			rPk{{$idx}} := {{$target.PKValue (print "c" $idx) $.ModelPkgAlias}}
			key{{$idx}} := seenKey[{{$.PKType $.ModelPkgAlias}}, {{$target.PKType $.ModelPkgAlias}}]{parent: pPK, child: rPk{{$idx}}}
			if _, ok := seen_{{$rel.FieldName}}[key{{$idx}}]; !ok {
				seen_{{$rel.FieldName}}[key{{$idx}}] = struct{}{}
				child := c{{$idx}}
				{{if eq $rel.Type "HasMany" -}}
				{{if $rel.IsPointer -}}
				parent.{{$rel.FieldName}} = append(parent.{{$rel.FieldName}}, &child)
				{{else -}}
				parent.{{$rel.FieldName}} = append(parent.{{$rel.FieldName}}, child)
				{{end -}}
				{{else if eq $rel.Type "BelongsTo" -}}
				{{if $rel.IsPointer -}}
				parent.{{$rel.FieldName}} = &child
				{{else -}}
				parent.{{$rel.FieldName}} = child
				{{end -}}
				{{end -}}
			}
		}
		{{end -}}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetchAll{{.NamePlural}}WithRelations: iterating rows: %w", err)
	}

	return items, nil
}
{{else}}
func get{{.Name}}ByIDWithRelations(ctx context.Context, db DBTX, {{.PKParams .ModelPkgAlias}}, cfg QueryOptions) (*{{.ModelPkgAlias}}.{{.Name}}, error) {
	opts := []QueryOption{Preload(false)}
	if cfg.IncludeDeleted {
		opts = append(opts, IncludeDeleted())
	}

	m, err := Get{{.Name}}ByID(ctx, db, {{.PKArgs ""}}, opts...)
	if err != nil {
		return nil, err
	}

	{{range $idx, $rel := .Relations -}}
	{{$target := index $.AllKnownModels $rel.TargetModel -}}
	{{if eq $rel.Type "HasMany"}}
	{
		preloadOpts := []QueryOption{Where("{{$rel.ForeignKey}} = $1", m.{{$.FirstPK.Name}})}
		if cfg.IncludeDeleted {
			preloadOpts = append(preloadOpts, IncludeDeleted())
		}
		children, err := FetchAll{{$target.NamePlural}}(ctx, db, preloadOpts...)
		if err != nil {
			return nil, fmt.Errorf("get{{$.Name}}ByIDWithRelations: preloading {{$rel.FieldName}}: %w", err)
		}
		{{if $rel.IsPointer -}}
		m.{{$rel.FieldName}} = children
		{{else -}}
		m.{{$rel.FieldName}} = make([]{{$.ModelPkgAlias}}.{{$target.Name}}, len(children))
		for i, child := range children {
			m.{{$rel.FieldName}}[i] = *child
		}
		{{end -}}
	}
	{{else if eq $rel.Type "BelongsTo"}}
	{{$parentFK := $.FieldByColumn $rel.ForeignKey -}}
	{{if $parentFK -}}
	{{if $parentFK.IsPointer}}
	if m.{{$parentFK.Name}} != nil {
		preloadOpts := []QueryOption{}
		if cfg.IncludeDeleted {
			preloadOpts = append(preloadOpts, IncludeDeleted())
		}
		child, err := Get{{$target.Name}}ByID(ctx, db, *m.{{$parentFK.Name}}, preloadOpts...)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get{{$.Name}}ByIDWithRelations: preloading {{$rel.FieldName}}: %w", err)
		}
		if child != nil {
			{{if $rel.IsPointer -}}
			m.{{$rel.FieldName}} = child
			{{else -}}
			m.{{$rel.FieldName}} = *child
			{{end -}}
		}
	}
	{{else}}
	if !IsZero(m.{{$parentFK.Name}}) {
		preloadOpts := []QueryOption{}
		if cfg.IncludeDeleted {
			preloadOpts = append(preloadOpts, IncludeDeleted())
		}
		child, err := Get{{$target.Name}}ByID(ctx, db, m.{{$parentFK.Name}}, preloadOpts...)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get{{$.Name}}ByIDWithRelations: preloading {{$rel.FieldName}}: %w", err)
		}
		if child != nil {
			{{if $rel.IsPointer -}}
			m.{{$rel.FieldName}} = child
			{{else -}}
			m.{{$rel.FieldName}} = *child
			{{end -}}
		}
	}
	{{end -}}
	{{end -}}
	{{end -}}
	{{end -}}

	return m, nil
}

func fetchAll{{.NamePlural}}WithRelations(ctx context.Context, db DBTX, clause string, args []any, cfg QueryOptions) ([]*{{.ModelPkgAlias}}.{{.Name}}, error) {
	query := "SELECT {{.AllColumns ""}} FROM {{.Table}}" + clause
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fetchAll{{.NamePlural}}WithRelations: %w", err)
	}
	defer rows.Close()

	items := make([]*{{.ModelPkgAlias}}.{{.Name}}, 0, 16)
	for rows.Next() {
		var m {{.ModelPkgAlias}}.{{.Name}}
		if err := rows.Scan({{.AllScanArgs "m"}}); err != nil {
			return nil, fmt.Errorf("fetchAll{{.NamePlural}}WithRelations: scanning row: %w", err)
		}
		items = append(items, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetchAll{{.NamePlural}}WithRelations: iterating rows: %w", err)
	}
	if len(items) == 0 {
		return items, nil
	}

	{{if .HasManyRelations -}}
	parentIDs := make([]any, 0, len(items))
	parentMap := make(map[{{.FirstPK.QualifiedType .ModelPkgAlias}}]*{{.ModelPkgAlias}}.{{.Name}}, len(items))
	for _, item := range items {
		pk := item.{{.FirstPK.Name}}
		if !IsZero(pk) {
			if _, exists := parentMap[pk]; !exists {
				parentMap[pk] = item
				parentIDs = append(parentIDs, pk)
			}
		}
	}
	{{end -}}

	{{range $idx, $rel := .Relations -}}
	{{$target := index $.AllKnownModels $rel.TargetModel -}}
	{{if eq $rel.Type "HasMany"}}
	{{$targetFK := $target.FieldByColumn $rel.ForeignKey -}}
	{{if $targetFK}}
	if len(parentIDs) > 0 {
		preloadOpts := []QueryOption{}
		if cfg.IncludeDeleted {
			preloadOpts = append(preloadOpts, IncludeDeleted())
		}
		children, err := batchIn(ctx, db, parentIDs, func(ctx context.Context, db DBTX, batch []any) ([]*{{$.ModelPkgAlias}}.{{$target.Name}}, error) {
			opts := append([]QueryOption{In("{{$rel.ForeignKey}}", batch...)}, preloadOpts...)
			return FetchAll{{$target.NamePlural}}(ctx, db, opts...)
		})
		if err != nil {
			return nil, fmt.Errorf("fetchAll{{$.NamePlural}}WithRelations: preloading {{$rel.FieldName}}: %w", err)
		}
		for _, child := range children {
			{{if $targetFK.IsPointer -}}
			if child.{{$targetFK.Name}} != nil {
				if parent, ok := parentMap[*child.{{$targetFK.Name}}]; ok {
					{{if $rel.IsPointer -}}
					parent.{{$rel.FieldName}} = append(parent.{{$rel.FieldName}}, child)
					{{else -}}
					parent.{{$rel.FieldName}} = append(parent.{{$rel.FieldName}}, *child)
					{{end -}}
				}
			}
			{{else -}}
			if parent, ok := parentMap[child.{{$targetFK.Name}}]; ok {
				{{if $rel.IsPointer -}}
				parent.{{$rel.FieldName}} = append(parent.{{$rel.FieldName}}, child)
				{{else -}}
				parent.{{$rel.FieldName}} = append(parent.{{$rel.FieldName}}, *child)
				{{end -}}
			}
			{{end -}}
		}
	}
	{{end -}}
	{{else if eq $rel.Type "BelongsTo"}}
	{{$parentFK := $.FieldByColumn $rel.ForeignKey -}}
	{{if $parentFK}}
	{
		fkIDs := make([]any, 0, len(items))
		fkMap := make(map[{{$target.FirstPK.QualifiedType $.ModelPkgAlias}}][]*{{$.ModelPkgAlias}}.{{$.Name}}, len(items))
		for _, item := range items {
			{{if $parentFK.IsPointer -}}
			if item.{{$parentFK.Name}} != nil {
				fk := *item.{{$parentFK.Name}}
				if _, exists := fkMap[fk]; !exists {
					fkIDs = append(fkIDs, fk)
				}
				fkMap[fk] = append(fkMap[fk], item)
			}
			{{else -}}
			fk := item.{{$parentFK.Name}}
			if !IsZero(fk) {
				if _, exists := fkMap[fk]; !exists {
					fkIDs = append(fkIDs, fk)
				}
				fkMap[fk] = append(fkMap[fk], item)
			}
			{{end -}}
		}

		if len(fkIDs) > 0 {
			preloadOpts := []QueryOption{}
			if cfg.IncludeDeleted {
				preloadOpts = append(preloadOpts, IncludeDeleted())
			}
			children, err := batchIn(ctx, db, fkIDs, func(ctx context.Context, db DBTX, batch []any) ([]*{{$.ModelPkgAlias}}.{{$target.Name}}, error) {
				opts := append([]QueryOption{In("{{$target.FirstPK.Column}}", batch...)}, preloadOpts...)
				return FetchAll{{$target.NamePlural}}(ctx, db, opts...)
			})
			if err != nil {
				return nil, fmt.Errorf("fetchAll{{$.NamePlural}}WithRelations: preloading {{$rel.FieldName}}: %w", err)
			}
			for _, child := range children {
				parents := fkMap[child.{{$target.FirstPK.Name}}]
				for _, parent := range parents {
					{{if $rel.IsPointer -}}
					parent.{{$rel.FieldName}} = child
					{{else -}}
					parent.{{$rel.FieldName}} = *child
					{{end -}}
				}
			}
		}
	}
	{{end -}}
	{{end -}}
	{{end -}}

	return items, nil
}
{{end}}
{{end}}
`
