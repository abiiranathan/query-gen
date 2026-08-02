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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"unicode"

	"github.com/abiiranathan/query-gen/inflection"
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

	// RelManyToMany marks a many-to-many relationship backed by a join table.
	RelManyToMany RelationType = "ManyToMany"
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

// Relation describes a HasMany, BelongsTo, or ManyToMany association discovered on a
// model, used to execute association preloading when requested.
type Relation struct {
	FieldName      string       // Struct field name on the parent, e.g. "User" or "Orders".
	TargetModel    string       // Related model type name, e.g. "User" or "Order".
	Type           RelationType // Cardinality type: HasMany, BelongsTo, or ManyToMany.
	ForeignKey     string       // Column name storing the reference ID.
	References     string       // Column name being referenced.
	IsPointer      bool         // True if the field is a pointer (*User vs User).
	OnDelete       string       // Referential action from gorm constraint tag, e.g. "CASCADE", "SET NULL". Empty means database default.
	OnUpdate       string       // Referential action from gorm constraint tag, e.g. "CASCADE", "RESTRICT". Empty means database default.
	JoinTable      string       // Pivot table name from gorm:"many2many:<table>".
	JoinForeignKey string       // Column in the join table referencing the parent model.
	JoinReferences string       // Column in the join table referencing the target model.
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
// HasMany/BelongsTo/ManyToMany relations to other known models.
type Model struct {
	Package        string           // Generated package name, e.g. "queries".
	ModelPkg       string           // Full import path of the source models package.
	ModelPkgAlias  string           // Import alias for the source models package, e.g. "models".
	Name           string           // Go type name, e.g. "User".
	NamePlural     string           // Plural of name.
	Table          string           // Database table name, e.g. "users".
	Fields         []Field          // List of scalar database fields.
	PK             []*Field         // Primary key field references (supports single & composite primary keys).
	Relations      []Relation       // Discovered HasMany, BelongsTo, and ManyToMany relations.
	AllKnownModels map[string]Model // All models in the parsed package, keyed by type name.
	TableChecks    []checkInfo      // Struct-level CHECK constraints not scoped to a single column.
}

// HasPK reports whether the model has at least one primary key defined.
func (m Model) HasPK() bool {
	return len(m.PK) > 0
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
// Fast path operates directly on ASCII bytes with zero allocations.
func toLowerCamel(s string) string {
	if s == "" {
		return ""
	}

	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return toLowerCamelUnicode(s)
		}
	}

	i := 0
	for i < len(s) && (s[i] >= 'A' && s[i] <= 'Z') {
		i++
	}
	if i == 0 {
		return s
	}
	if i == len(s) {
		return strings.ToLower(s)
	}

	b := []byte(s)
	if i > 1 {
		for j := 0; j < i-1; j++ {
			b[j] = b[j] + ('a' - 'A')
		}
	} else {
		b[0] = b[0] + ('a' - 'A')
	}
	return string(b)
}

func toLowerCamelUnicode(s string) string {
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
		return fmt.Sprintf("ScanNullable(&%s.%s)", varName, f.Name)
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
	sb.Grow(len(m.Relations) * 64)
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
	joins := make([]string, 0, len(m.Relations))
	for i, rel := range m.Relations {
		alias := fmt.Sprintf("r%d", i)
		target, ok := m.AllKnownModels[rel.TargetModel]
		if !ok {
			continue
		}

		var joinCond string
		switch rel.Type {
		case RelHasMany:
			if m.FirstPK() == nil {
				continue
			}
			joinCond = fmt.Sprintf("LEFT JOIN %s %s ON %s.%s = %s.%s",
				target.Table, alias, alias, rel.ForeignKey, parentPrefix, m.FirstPK().Column)
		case RelBelongsTo:
			if target.FirstPK() == nil {
				continue
			}
			joinCond = fmt.Sprintf("LEFT JOIN %s %s ON %s.%s = %s.%s",
				target.Table, alias, alias, target.FirstPK().Column, parentPrefix, rel.ForeignKey)
		case RelManyToMany:
			if m.FirstPK() == nil || target.FirstPK() == nil {
				continue
			}
			pivotAlias := fmt.Sprintf("j%d", i)
			joinCond = fmt.Sprintf("LEFT JOIN %s %s ON %s.%s = %s.%s\n\t\tLEFT JOIN %s %s ON %s.%s = %s.%s",
				rel.JoinTable, pivotAlias, pivotAlias, rel.JoinForeignKey, parentPrefix, m.FirstPK().Column,
				target.Table, alias, alias, target.FirstPK().Column, pivotAlias, rel.JoinReferences)
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

// ManyToManyRelations returns the subset of m.Relations with cardinality ManyToMany.
func (m Model) ManyToManyRelations() []Relation {
	out := make([]Relation, 0, len(m.Relations))
	for _, r := range m.Relations {
		if r.Type == RelManyToMany {
			out = append(out, r)
		}
	}
	return out
}

// UseJoinStrategy reports whether relation preloading should be generated as
// a single LEFT JOIN query.
func (m Model) UseJoinStrategy() bool {
	return len(m.HasManyRelations())+len(m.ManyToManyRelations()) <= 1
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
	defaultNullable := flag.Bool("nullable", false, "All fields without explicit \"not null\" tag are considered NULL")
	flag.Parse()

	parsedModels, fullPkgPath, err := parsePackage(*inputPkg, *defaultNullable)
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

	// Bounded worker pool matching CPU count to execute template generation,
	// goimports formatting, and file I/O in parallel across all parsed models.
	numWorkers := min(runtime.GOMAXPROCS(0), len(parsedModels))

	jobs := make(chan Model, len(parsedModels))
	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		genErr  error
	)

	for range numWorkers {
		wg.Go(func() {
			for model := range jobs {
				model.Package = *outPkg
				model.ModelPkg = fullPkgPath
				model.ModelPkgAlias = pkgAlias
				model.AllKnownModels = modelMap

				code, err := generateQueries(model)
				if err != nil {
					errOnce.Do(func() {
						genErr = fmt.Errorf("query-gen: generating code for %q: %w", model.Name, err)
					})
					return
				}

				fileName := fmt.Sprintf("%s_gen.go", toSnakeCase(model.Name))
				filePath := filepath.Join(*outDir, fileName)

				formatted, err := imports.Process(filePath, []byte(code), nil)
				if err != nil {
					log.Printf("query-gen: warning: goimports failed for %q: %v (writing unformatted source)", model.Name, err)
					formatted = []byte(code)
				}

				if err := os.WriteFile(filePath, formatted, 0o644); err != nil {
					errOnce.Do(func() {
						genErr = fmt.Errorf("query-gen: writing %q: %v", filePath, err)
					})
					return
				}
			}
		})
	}

	for _, model := range parsedModels {
		jobs <- model
	}
	close(jobs)
	wg.Wait()

	if genErr != nil {
		log.Fatal(genErr)
	}
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

func safePlural(s string) string {
	plural := inflection.Plural(s)
	if plural == s {
		// "Information" -> "InformationList"
		return s + "List"
	}
	return plural
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

// findEnclosingGenDecl finds the *ast.GenDecl in file that declares typeSpec.
// This is needed because doc comments on grouped type declarations
// (type ( Foo struct{} )) attach to the GenDecl, not the TypeSpec.
func findEnclosingGenDecl(file *ast.File, target *ast.TypeSpec) (*ast.GenDecl, bool) {
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			if spec == target {
				return genDecl, true
			}
		}
	}
	return nil, false
}

func parsePackage(pattern string, defaultNullable bool) ([]Model, string, error) {
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
		var tableNames map[string]string
		var customDataTypes map[string]string

		// Gather table names and custom data types concurrently
		var wg sync.WaitGroup
		wg.Go(func() {
			tableNames = collectTableNameMethods(pkg.Syntax)
		})
		wg.Go(func() {
			customDataTypes = collectDataTypeMethods(pkg)
		})
		wg.Wait()

		// Inspect AST syntax files in parallel while preserving file declaration order
		type fileModels struct {
			models []Model
		}

		fileResults := make([]fileModels, len(pkg.Syntax))
		var wgFiles sync.WaitGroup

		for idx, file := range pkg.Syntax {
			wgFiles.Add(1)
			go func(idx int, file *ast.File) {
				defer wgFiles.Done()
				var localModels []Model

				ast.Inspect(file, func(n ast.Node) bool {
					typeSpec, ok := n.(*ast.TypeSpec)
					if !ok || !typeSpec.Name.IsExported() {
						return true
					}

					structType, ok := typeSpec.Type.(*ast.StructType)
					if !ok {
						return true
					}

					doc := typeSpec.Doc
					if doc == nil {
						if genDecl, ok := findEnclosingGenDecl(file, typeSpec); ok {
							doc = genDecl.Doc
						}
					}
					if doc != nil && strings.Contains(doc.Text(), "query-gen: skip") {
						return true
					}

					model := Model{
						Name:       typeSpec.Name.Name,
						NamePlural: safePlural(typeSpec.Name.Name),
						Table:      inflection.Plural(toSnakeCase(typeSpec.Name.Name)),
					}

					if explicit, ok := tableNames[model.Name]; ok {
						model.Table = explicit
					}

					model.Fields = make([]Field, 0, len(structType.Fields.List))
					model.Relations = make([]Relation, 0, 4)

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
						parsedField := parseFieldTags(fieldName, fieldType, rawTag, defaultNullable)

						if parsedField.IsIgnore {
							continue
						}

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
						return true
					}

					localModels = append(localModels, model)
					return true
				})

				fileResults[idx].models = localModels
			}(idx, file)
		}
		wgFiles.Wait()

		for _, res := range fileResults {
			result = append(result, res.models...)
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
			case strings.HasPrefix(part, "many2many:"):
				rel.Type = RelManyToMany
				rel.JoinTable = strings.TrimPrefix(part, "many2many:")
			case strings.HasPrefix(part, "joinForeignKey:"):
				rel.JoinForeignKey = toSnakeCase(strings.TrimPrefix(part, "joinForeignKey:"))
			case strings.HasPrefix(part, "joinReferences:"):
				rel.JoinReferences = toSnakeCase(strings.TrimPrefix(part, "joinReferences:"))
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
	case RelManyToMany:
		if rel.JoinForeignKey == "" {
			rel.JoinForeignKey = toSnakeCase(parentModelName) + "_id"
		}
		if rel.JoinReferences == "" {
			rel.JoinReferences = toSnakeCase(rel.TargetModel) + "_id"
		}
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

func parseFieldTags(fieldName, fieldType, rawTag string, defaultNullable bool) Field {
	f := Field{
		Name:       fieldName,
		Type:       fieldType,
		Column:     toSnakeCase(fieldName),
		IsPK:       strings.EqualFold(fieldName, "ID"),
		Permission: PermReadWrite,   // rw for field by default
		Nullable:   defaultNullable, // Nullable by default (unless PK or not null specified)
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

	for part := range strings.SplitSeq(gormTag, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), "column:") {
			f.Column = part[len("column:"):]
			break
		}
	}

	canRead := true
	canCreate := true
	canUpdate := true
	hasReadTag := false
	hasWriteTag := false

	for part := range strings.SplitSeq(gormTag, ";") {
		part = strings.TrimSpace(part)
		lower := strings.ToLower(part)

		switch {
		case part == "-" || lower == "-:all":
			f.IsIgnore = true
			return f

		case lower == "-:migration":

		case lower == "primarykey" || lower == "primary_key":
			f.IsPK = true

		case lower == "unique":
			f.IsUnique = true

		case lower == "index":
			f.HasIndex = true

		case strings.HasPrefix(lower, "index:"):
			f.HasIndex = true
			f.IndexName = part[len("index:"):]

		case lower == "uniqueindex" || lower == "unique_index":
			f.HasUniqueIndex = true

		case strings.HasPrefix(lower, "uniqueindex:"):
			f.HasUniqueIndex = true
			f.UniqueIndexName = part[len("uniqueindex:"):]

		case strings.HasPrefix(lower, "unique_index:"):
			f.HasUniqueIndex = true
			f.UniqueIndexName = part[len("unique_index:"):]

		case strings.HasPrefix(lower, "default:"):
			f.HasDefault = true
			f.DefaultVal = part[len("default:"):]

		case strings.HasPrefix(lower, "type:"):
			f.RawSQLType = part[len("type:"):]

		case strings.HasPrefix(lower, "datatype:"):
			if f.RawSQLType == "" {
				f.RawSQLType = part[len("datatype:"):]
			}

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

		case lower == "not null":
			f.Nullable = false

		case lower == "null":
			f.Nullable = true

		case lower == "->" || lower == "->:true" || lower == "->:rw" || lower == "->:r":
			canRead = true
			hasReadTag = true

		case lower == "->:false":
			canRead = false
			hasReadTag = true

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

	if hasReadTag && !hasWriteTag && canRead {
		canCreate = false
		canUpdate = false
	}

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

	if f.IsPK {
		f.Nullable = false
	}

	return f
}

// parsedTemplate caches compiled text template lazily on first access.
// Safe for concurrent use by multiple goroutines.
var parsedTemplate = sync.OnceValue(func() *template.Template {
	return template.Must(template.New("gen").Parse(codeTemplate))
})

// generateQueries renders code for the specified model using the cached template.
// Safe for concurrent use by multiple goroutines.
func generateQueries(m Model) (string, error) {
	tmpl := parsedTemplate()
	var buf bytes.Buffer
	buf.Grow(4096 * 2)
	if err := tmpl.Execute(&buf, m); err != nil {
		return "", fmt.Errorf("executing code template for %q: %w", m.Name, err)
	}
	return buf.String(), nil
}

// toSnakeCase converts a CamelCase Go identifier to snake_case.
// Fast path processes standard ASCII strings with zero heap allocations.
func toSnakeCase(str string) string {
	if str == "" {
		return ""
	}

	for i := 0; i < len(str); i++ {
		if str[i] > 127 {
			return toSnakeCaseUnicode(str)
		}
	}

	var b strings.Builder
	b.Grow(len(str) + 4)
	length := len(str)

	for i := range length {
		c := str[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				prev := str[i-1]
				nextIsLower := i+1 < length && (str[i+1] >= 'a' && str[i+1] <= 'z')
				prevIsLower := prev >= 'a' && prev <= 'z'
				prevIsUpper := prev >= 'A' && prev <= 'Z'
				if prevIsLower || (nextIsLower && prevIsUpper) {
					b.WriteByte('_')
				}
			}
			b.WriteByte(c + ('a' - 'A'))
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func toSnakeCaseUnicode(str string) string {
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

// --- Model Code Template ---
