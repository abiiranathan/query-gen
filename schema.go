package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// SchemaDialect identifies the target SQL dialect for schema generation.
type SchemaDialect string

const (
	// DialectPostgres represents PostgreSQL dialect.
	DialectPostgres SchemaDialect = "postgres"
	// DialectSQLite represents SQLite dialect.
	DialectSQLite SchemaDialect = "sqlite3"
)

// sqlType maps a Go field type and dialect to a column type declaration,
// honoring an explicit size/precision override when present (e.g. from
// gorm:"type:varchar(255)" or gorm:"size:255").
func sqlType(f Field, dialect SchemaDialect) string {
	// An explicit raw type tag always wins — the caller has taken over
	// responsibility for dialect correctness.
	if f.RawSQLType != "" {
		return f.RawSQLType
	}

	base := f.UnderlyingBaseType()

	switch dialect {
	case DialectPostgres:
		switch base {
		case "time.Time":
			return "TIMESTAMPTZ"
		case "time.Duration":
			return "BIGINT"
		case "string":
			if f.Size > 0 {
				return fmt.Sprintf("VARCHAR(%d)", f.Size)
			}
			return "TEXT"
		case "bool":
			return "BOOLEAN"
		case "float32":
			return "REAL"
		case "float64":
			if f.Precision > 0 {
				if f.Scale > 0 {
					return fmt.Sprintf("NUMERIC(%d,%d)", f.Precision, f.Scale)
				}
				return fmt.Sprintf("NUMERIC(%d)", f.Precision)
			}
			return "DOUBLE PRECISION"
		case "int8", "int16":
			return "SMALLINT"
		case "int32":
			return "INTEGER"
		case "int", "int64":
			return "BIGINT"
		case "uint8", "uint16":
			return "SMALLINT"
		case "uint32":
			return "INTEGER"
		case "uint", "uint64":
			return "BIGINT"
		case "[]byte":
			return "BYTEA"
		default:
			return "TEXT"
		}
	case DialectSQLite:
		switch {
		case base == "time.Time":
			return "TEXT"
		case base == "time.Duration":
			return "INT"
		case base == "string":
			if f.Size > 0 {
				return fmt.Sprintf("VARCHAR(%d)", f.Size)
			}
			return "TEXT"
		case base == "bool":
			return "BOOLEAN"
		case base == "float32", base == "float64":
			return "REAL"
		case strings.HasPrefix(base, "int"), strings.HasPrefix(base, "uint"):
			return "INTEGER"
		case base == "[]byte":
			return "BLOB"
		default:
			return "TEXT"
		}
	default:
		return "TEXT"
	}
}

// fkSqlType maps a primary key field type to its corresponding SQL foreign key type.
func fkSqlType(f Field, dialect SchemaDialect) string {
	base := f.UnderlyingBaseType()
	switch dialect {
	case DialectPostgres:
		switch base {
		case "int32", "uint32":
			return "INTEGER"
		case "int", "int64", "uint", "uint64":
			return "BIGINT"
		default:
			return sqlType(f, dialect)
		}
	case DialectSQLite:
		switch base {
		case "int", "int64", "int32", "uint", "uint64", "uint32":
			return "INTEGER"
		default:
			return sqlType(f, dialect)
		}
	default:
		return sqlType(f, dialect)
	}
}

// formatDefaultValue formats a raw default value string into valid SQL for the target dialect.
func formatDefaultValue(val string, _ Field, dialect SchemaDialect) string {
	val = strings.TrimSpace(val)
	if val == "" {
		return ""
	}

	lower := strings.ToLower(val)

	if lower == "null" {
		return "DEFAULT NULL"
	}

	if lower == "now()" || lower == "current_timestamp" {
		switch dialect {
		case DialectPostgres:
			return "DEFAULT now()"
		case DialectSQLite:
			return "DEFAULT CURRENT_TIMESTAMP"
		}
	}

	if lower == "true" || lower == "false" {
		switch dialect {
		case DialectSQLite:
			if lower == "true" {
				return "DEFAULT 1"
			}
			return "DEFAULT 0"
		default:
			return "DEFAULT " + lower
		}
	}

	if (strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) ||
		strings.Contains(val, "(") ||
		strings.Contains(val, "::") ||
		isNumeric(val) {
		return "DEFAULT " + val
	}

	return fmt.Sprintf("DEFAULT '%s'", strings.ReplaceAll(val, "'", "''"))
}

// isNumeric reports whether a string represents a valid integer or floating-point number.
func isNumeric(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// indexInfo aggregates composite or single-column index definitions.
type indexInfo struct {
	Name    string
	Columns []string
	Unique  bool
}

// checkInfo represents a CHECK constraint attached to a model, either
// column-scoped (e.g. gorm:"check:amount > 0") or table-scoped.
type checkInfo struct {
	Name string
	Expr string
}

// pkColumns returns the ordered list of primary-key column names for the
// model, supporting composite primary keys (multiple fields tagged
// gorm:"primaryKey").
func (m Model) pkColumns() []string {
	cols := make([]string, 0, len(m.PK))
	for _, pk := range m.PK {
		cols = append(cols, pk.Column)
	}
	return cols
}

// isCompositePK reports whether the model has more than one primary key column.
func (m Model) isCompositePK() bool {
	return len(m.pkColumns()) > 1
}

// uniquenessLabel renders a human-readable description of an index's
// uniqueness for use in collision error messages.
func uniquenessLabel(unique bool) string {
	if unique {
		return "UNIQUE"
	}
	return "non-unique"
}

// GenerateSchema renders CREATE TABLE, CHECK, and CREATE INDEX SQL statements for a single model.
func (m Model) GenerateSchema(dialect SchemaDialect) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "CREATE TABLE IF NOT EXISTS %s (\n", m.Table)

	lines := make([]string, 0, len(m.Fields)+len(m.Relations)+1)
	composite := m.isCompositePK()

	for _, f := range m.Fields {
		var col strings.Builder
		col.WriteString("    ")
		col.WriteString(f.Column)
		col.WriteByte(' ')
		col.WriteString(sqlType(f, dialect))

		switch {
		case f.IsPK && !composite:
			// Single-column PK: use auto-increment identity types where applicable.
			switch dialect {
			case DialectPostgres:
				switch f.UnderlyingBaseType() {
				case "int", "int64", "uint", "uint64":
					col.Reset()
					col.WriteString("    ")
					col.WriteString(f.Column)
					col.WriteString(" BIGSERIAL PRIMARY KEY")
				case "int32", "uint32":
					col.Reset()
					col.WriteString("    ")
					col.WriteString(f.Column)
					col.WriteString(" SERIAL PRIMARY KEY")
				default:
					col.WriteString(" PRIMARY KEY")
				}
			case DialectSQLite:
				switch f.UnderlyingBaseType() {
				case "int", "int64", "int32", "uint", "uint64", "uint32":
					col.Reset()
					col.WriteString("    ")
					col.WriteString(f.Column)
					col.WriteString(" INTEGER PRIMARY KEY AUTOINCREMENT")
				default:
					col.WriteString(" PRIMARY KEY")
				}
			}
		case f.IsPK && composite:
			// Composite PK: no auto-increment/identity; constraint is emitted
			// as a separate table-level PRIMARY KEY(...) clause below.
			col.WriteString(" NOT NULL")
		default:
			if !f.Nullable {
				col.WriteString(" NOT NULL")
			}
			if f.IsUnique {
				col.WriteString(" UNIQUE")
			}

			if f.HasDefault && f.DefaultVal != "" {
				col.WriteByte(' ')
				col.WriteString(formatDefaultValue(f.DefaultVal, f, dialect))
			} else if strings.EqualFold(f.Name, "CreatedAt") && f.IsTimestamp() {
				rawType := strings.TrimSpace(strings.ToLower(f.RawSQLType))
				switch dialect {
				case DialectPostgres:
					if rawType == "date" {
						col.WriteString(" DEFAULT CURRENT_DATE")
					} else {
						col.WriteString(" DEFAULT CURRENT_TIMESTAMP") // or " DEFAULT now()"
					}
				case DialectSQLite:
					if rawType == "date" {
						col.WriteString(" DEFAULT CURRENT_DATE")
					} else {
						col.WriteString(" DEFAULT CURRENT_TIMESTAMP")
					}
				}
			}

			if f.CheckConstraint != "" {
				fmt.Fprintf(&col, " CHECK (%s)", f.CheckConstraint)
			}
		}

		lines = append(lines, col.String())
	}

	// Table-level composite primary key.
	if composite {
		lines = append(lines, fmt.Sprintf("    PRIMARY KEY (%s)", strings.Join(m.pkColumns(), ", ")))
	}

	// Foreign key constraints derived from BelongsTo relations.
	for _, rel := range m.Relations {
		if rel.Type != RelBelongsTo {
			continue
		}
		target, ok := m.AllKnownModels[rel.TargetModel]
		if !ok || len(target.PK) == 0 {
			continue
		}
		refCol := rel.References
		if refCol == "" {
			if target.isCompositePK() {
				log.Fatalf("query-gen: model %q relation %q references model %q which has a composite primary key, but no explicit 'references' tag was specified",
					m.Name, rel.FieldName, target.Name)
			}
			refCol = target.PK[0].Column
		}

		fkLine := fmt.Sprintf("    CONSTRAINT fk_%s_%s FOREIGN KEY (%s) REFERENCES %s(%s)",
			m.Table, rel.ForeignKey, rel.ForeignKey, target.Table, refCol)

		if rel.OnDelete != "" {
			fkLine += " ON DELETE " + strings.ToUpper(rel.OnDelete)
		}
		if rel.OnUpdate != "" {
			fkLine += " ON UPDATE " + strings.ToUpper(rel.OnUpdate)
		}

		lines = append(lines, fkLine)
	}

	// Table-level CHECK constraints (e.g. from gorm:"check:name,expr" at struct level).
	for _, chk := range m.TableChecks {
		name := chk.Name
		if name == "" {
			name = fmt.Sprintf("chk_%s_%d", m.Table, len(lines))
		}
		lines = append(lines, fmt.Sprintf("    CONSTRAINT %s CHECK (%s)", name, chk.Expr))
	}

	sb.WriteString(strings.Join(lines, ",\n"))
	sb.WriteString("\n);\n")

	// Collect and aggregate regular & unique index declarations (including composite indexes).
	indices := make(map[string]*indexInfo)
	indexOrder := make([]string, 0)

	getOrCreateIndex := func(name string, unique bool) *indexInfo {
		if idx, ok := indices[name]; ok {
			if idx.Unique != unique {
				log.Fatalf("query-gen: model %q has conflicting index %q: previously declared %s, now requested %s",
					m.Name, name, uniquenessLabel(idx.Unique), uniquenessLabel(unique))
			}
			return idx
		}
		idx := &indexInfo{Name: name, Unique: unique, Columns: make([]string, 0, 2)}
		indices[name] = idx
		indexOrder = append(indexOrder, name)
		return idx
	}

	columnHasLeadingIndex := func(column string) bool {
		for _, idx := range indices {
			if len(idx.Columns) > 0 && idx.Columns[0] == column {
				return true
			}
		}
		return false
	}

	for _, f := range m.Fields {
		if f.HasIndex {
			name := f.IndexName
			if name == "" {
				name = fmt.Sprintf("idx_%s_%s", m.Table, f.Column)
			}
			idx := getOrCreateIndex(name, false)
			idx.Columns = append(idx.Columns, f.Column)
		}
		if f.HasUniqueIndex {
			name := f.UniqueIndexName
			if name == "" {
				name = fmt.Sprintf("uidx_%s_%s", m.Table, f.Column)
			}
			idx := getOrCreateIndex(name, true)
			idx.Columns = append(idx.Columns, f.Column)
		}
	}

	if m.HasDeletedAt() {
		deletedAtCol := m.DeletedAtField().Column
		if !columnHasLeadingIndex(deletedAtCol) {
			deletedAtIdxName := fmt.Sprintf("idx_%s_%s", m.Table, deletedAtCol)
			idx := getOrCreateIndex(deletedAtIdxName, false)
			idx.Columns = append(idx.Columns, deletedAtCol)
		}
	}

	for _, rel := range m.Relations {
		if rel.Type != RelBelongsTo {
			continue
		}
		if !columnHasLeadingIndex(rel.ForeignKey) {
			fkIdxName := fmt.Sprintf("idx_%s_%s", m.Table, rel.ForeignKey)
			idx := getOrCreateIndex(fkIdxName, false)
			idx.Columns = append(idx.Columns, rel.ForeignKey)
		}
	}

	for _, name := range indexOrder {
		idx := indices[name]
		uniqueStr := ""
		if idx.Unique {
			uniqueStr = "UNIQUE "
		}
		fmt.Fprintf(&sb, "CREATE %sINDEX IF NOT EXISTS %s ON %s (%s);\n",
			uniqueStr, idx.Name, m.Table, strings.Join(idx.Columns, ", "))
	}

	return sb.String()
}

// GenerateJoinTableSchema renders CREATE TABLE and INDEX SQL statements for a many-to-many join table.
func GenerateJoinTableSchema(rel Relation, parent Model, modelMap map[string]Model, dialect SchemaDialect) string {
	target, ok := modelMap[rel.TargetModel]
	if !ok || parent.FirstPK() == nil || target.FirstPK() == nil {
		return ""
	}

	parentPKCol := parent.FirstPK().Column
	targetPKCol := target.FirstPK().Column

	parentFKType := fkSqlType(*parent.FirstPK(), dialect)
	targetFKType := fkSqlType(*target.FirstPK(), dialect)

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE TABLE IF NOT EXISTS %s (\n", rel.JoinTable)
	fmt.Fprintf(&sb, "    %s %s NOT NULL,\n", rel.JoinForeignKey, parentFKType)
	fmt.Fprintf(&sb, "    %s %s NOT NULL,\n", rel.JoinReferences, targetFKType)

	onDelete := "CASCADE"
	if rel.OnDelete != "" {
		onDelete = strings.ToUpper(rel.OnDelete)
	}

	onUpdate := ""
	if rel.OnUpdate != "" {
		onUpdate = " ON UPDATE " + strings.ToUpper(rel.OnUpdate)
	}

	fmt.Fprintf(&sb, "    CONSTRAINT fk_%s_%s FOREIGN KEY (%s) REFERENCES %s(%s) ON DELETE %s%s,\n",
		rel.JoinTable, rel.JoinForeignKey, rel.JoinForeignKey, parent.Table, parentPKCol, onDelete, onUpdate)
	fmt.Fprintf(&sb, "    CONSTRAINT fk_%s_%s FOREIGN KEY (%s) REFERENCES %s(%s) ON DELETE %s%s,\n",
		rel.JoinTable, rel.JoinReferences, rel.JoinReferences, target.Table, targetPKCol, onDelete, onUpdate)
	fmt.Fprintf(&sb, "    PRIMARY KEY (%s, %s)\n", rel.JoinForeignKey, rel.JoinReferences)
	sb.WriteString(");\n")

	fmt.Fprintf(&sb, "CREATE INDEX IF NOT EXISTS idx_%s_%s ON %s (%s);\n",
		rel.JoinTable, rel.JoinReferences, rel.JoinTable, rel.JoinReferences)

	return sb.String()
}

// writeSchemaFile renders CREATE TABLE and INDEX statements for all models and writes to disk.
func writeSchemaFile(models []Model, modelMap map[string]Model, dialect SchemaDialect, outPath string) error {
	ordered := orderModelsByDependency(models, modelMap)

	var sb strings.Builder
	fmt.Fprintf(&sb, "-- Code generated by query-gen. DO NOT EDIT.\n-- Dialect: %s\n\n", dialect)

	writtenJoinTables := make(map[string]bool)

	for _, m := range ordered {
		// Skip tables without a primary key (views).
		if m.FirstPK() == nil {
			continue
		}

		m.AllKnownModels = modelMap
		sb.WriteString(m.GenerateSchema(dialect))
		sb.WriteByte('\n')

		for _, rel := range m.Relations {
			if rel.Type == RelManyToMany && rel.JoinTable != "" {
				if writtenJoinTables[rel.JoinTable] {
					continue
				}
				writtenJoinTables[rel.JoinTable] = true
				sb.WriteString(GenerateJoinTableSchema(rel, m, modelMap, dialect))
				sb.WriteByte('\n')
			}
		}
	}

	if err := os.WriteFile(outPath, []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("writing schema file %q: %w", outPath, err)
	}
	return nil
}

// orderModelsByDependency performs a topological sort so referenced tables
// are emitted before dependent tables. Self-referential BelongsTo relations
// (a model referencing its own table) are skipped during traversal to avoid
// false cycle detection, since a table can safely reference itself in SQL.
func orderModelsByDependency(models []Model, modelMap map[string]Model) []Model {
	visited := make(map[string]bool, len(models))
	result := make([]Model, 0, len(models))

	var visit func(m Model, stack map[string]bool)
	visit = func(m Model, stack map[string]bool) {
		if visited[m.Name] || stack[m.Name] {
			return
		}
		stack[m.Name] = true

		for _, rel := range m.Relations {
			if rel.Type != RelBelongsTo || rel.TargetModel == m.Name {
				continue
			}
			if target, ok := modelMap[rel.TargetModel]; ok {
				visit(target, stack)
			}
		}

		stack[m.Name] = false
		if !visited[m.Name] {
			visited[m.Name] = true
			result = append(result, m)
		}
	}

	for _, m := range models {
		// Skip models without PK (considered views)
		if m.FirstPK() == nil {
			continue
		}
		visit(m, make(map[string]bool))
	}

	return result
}
