package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/types"
	"log"
	"os"
	"reflect"
	"strings"
	"text/template"
	"unicode"

	"golang.org/x/tools/go/packages"
)

type Model struct {
	Package string
	Name    string
	Table   string
	Fields  []Field
	PK      *Field
}

type Field struct {
	Name     string
	Type     string
	Column   string
	IsPK     bool
	IsIgnore bool
}

// --- Template Helper Methods on Model ---

func (m Model) AllColumns() string {
	cols := make([]string, len(m.Fields))
	for i, f := range m.Fields {
		cols[i] = f.Column
	}
	return strings.Join(cols, ", ")
}

func (m Model) NonPKColumns() string {
	var cols []string
	for _, f := range m.Fields {
		if !f.IsPK {
			cols = append(cols, f.Column)
		}
	}
	return strings.Join(cols, ", ")
}

func (m Model) InsertPlaceholders() string {
	var placeholders []string
	idx := 1
	for _, f := range m.Fields {
		if !f.IsPK {
			placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
			idx++
		}
	}
	return strings.Join(placeholders, ", ")
}

func (m Model) UpdateSetClause() string {
	var clauses []string
	idx := 1
	for _, f := range m.Fields {
		if !f.IsPK {
			clauses = append(clauses, fmt.Sprintf("%s = $%d", f.Column, idx))
			idx++
		}
	}
	return strings.Join(clauses, ", ")
}

func (m Model) UpdatePKPlaceholderIdx() int {
	count := 1
	for _, f := range m.Fields {
		if !f.IsPK {
			count++
		}
	}
	return count
}

func (m Model) AllScanArgs(varName string) string {
	args := make([]string, len(m.Fields))
	for i, f := range m.Fields {
		args[i] = fmt.Sprintf("&%s.%s", varName, f.Name)
	}
	return strings.Join(args, ", ")
}

func (m Model) NonPKScanArgs(varName string) string {
	var args []string
	for _, f := range m.Fields {
		if !f.IsPK {
			args = append(args, fmt.Sprintf("&%s.%s", varName, f.Name))
		}
	}
	return strings.Join(args, ", ")
}

// --- Main Engine ---

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("Usage: go run main.go <package_path>")
	}
	pkgPath := os.Args[1]

	models, err := parsePackage(pkgPath)
	if err != nil {
		log.Fatalf("Error parsing package: %v", err)
	}

	for _, model := range models {
		code, err := generateQueries(model)
		if err != nil {
			log.Fatalf("Error generating code for %s: %v", model.Name, err)
		}
		fmt.Println(code)
	}
}

func parsePackage(pattern string) ([]Model, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
	}

	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return nil, err
	}
	if packages.PrintErrors(pkgs) > 0 {
		return nil, fmt.Errorf("package has errors")
	}

	var models []Model

	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				typeSpec, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}

				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					return true
				}

				model := Model{
					Package: pkg.Name,
					Name:    typeSpec.Name.Name,
					Table:   toSnakeCase(typeSpec.Name.Name) + "s",
				}

				for _, field := range structType.Fields.List {
					if len(field.Names) == 0 {
						continue // Skip embedded structs for now
					}

					fieldName := field.Names[0].Name
					fieldType := types.ExprString(field.Type)

					rawTag := ""
					if field.Tag != nil {
						rawTag = strings.Trim(field.Tag.Value, "`")
					}

					parsedField := parseFieldTags(fieldName, fieldType, rawTag)

					// Skip fields marked with gorm:"-"
					if parsedField.IsIgnore {
						continue
					}

					if parsedField.IsPK {
						model.PK = &parsedField
					}
					model.Fields = append(model.Fields, parsedField)
				}

				// Fallback to first field as PK if none specified
				if model.PK == nil && len(model.Fields) > 0 {
					model.PK = &model.Fields[0]
					model.PK.IsPK = true
				}

				models = append(models, model)
				return true
			})
		}
	}

	return models, nil
}

// parseFieldTags handles GORM tags as primary source, falling back to db tags
func parseFieldTags(fieldName, fieldType, rawTag string) Field {
	f := Field{
		Name:     fieldName,
		Type:     fieldType,
		Column:   toSnakeCase(fieldName),
		IsPK:     strings.EqualFold(fieldName, "ID"), // Default GORM convention
		IsIgnore: false,
	}

	if rawTag == "" {
		return f
	}

	structTag := reflect.StructTag(rawTag)

	// 1. Process GORM Tag
	if gormTag := structTag.Get("gorm"); gormTag != "" {
		parts := strings.Split(gormTag, ";")
		for _, part := range parts {
			part = strings.TrimSpace(part)

			if part == "-" {
				f.IsIgnore = true
				return f
			}

			if strings.EqualFold(part, "primaryKey") || strings.EqualFold(part, "primary_key") {
				f.IsPK = true
			}

			if strings.HasPrefix(strings.ToLower(part), "column:") {
				f.Column = part[7:]
			}
		}
		return f
	}

	// 2. Fallback: Process `db` Tag
	if dbTag := structTag.Get("db"); dbTag != "" {
		parts := strings.Split(dbTag, ",")
		if parts[0] != "" {
			f.Column = parts[0]
		}
		for _, opt := range parts[1:] {
			if opt == "pk" {
				f.IsPK = true
			}
		}
	}

	return f
}

const codeTemplate = `// Code generated by query-gen. DO NOT EDIT.
package {{.Package}}

import (
	"context"
	"database/sql"
)

// Insert{{.Name}} inserts a new record into {{.Table}}
func Insert{{.Name}}(ctx context.Context, db *sql.DB, m *{{.Name}}) error {
	query := ` + "`" + `
		INSERT INTO {{.Table}} ({{.NonPKColumns}})
		VALUES ({{.InsertPlaceholders}})
		RETURNING {{.PK.Column}}
	` + "`" + `
	return db.QueryRowContext(ctx, query, {{.NonPKScanArgs "m"}}).Scan(&m.{{.PK.Name}})
}

// Get{{.Name}}ByID retrieves a single record from {{.Table}} by Primary Key
func Get{{.Name}}ByID(ctx context.Context, db *sql.DB, id {{.PK.Type}}) (*{{.Name}}, error) {
	query := ` + "`" + `
		SELECT {{.AllColumns}}
		FROM {{.Table}}
		WHERE {{.PK.Column}} = $1
	` + "`" + `
	
	row := db.QueryRowContext(ctx, query, id)
	var m {{.Name}}
	err := row.Scan({{.AllScanArgs "m"}})
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Update{{.Name}} updates an existing record in {{.Table}}
func Update{{.Name}}(ctx context.Context, db *sql.DB, m *{{.Name}}) error {
	query := ` + "`" + `
		UPDATE {{.Table}}
		SET {{.UpdateSetClause}}
		WHERE {{.PK.Column}} = ${{.UpdatePKPlaceholderIdx}}
	` + "`" + `

	_, err := db.ExecContext(ctx, query, {{.NonPKScanArgs "m"}}, m.{{.PK.Name}})
	return err
}

// Delete{{.Name}} deletes a record from {{.Table}} by ID
func Delete{{.Name}}(ctx context.Context, db *sql.DB, id {{.PK.Type}}) error {
	query := ` + "`" + `DELETE FROM {{.Table}} WHERE {{.PK.Column}} = $1` + "`" + `
	_, err := db.ExecContext(ctx, query, id)
	return err
}

// List{{.Name}}s retrieves multiple records from {{.Table}}
func List{{.Name}}s(ctx context.Context, db *sql.DB, limit, offset int) ([]*{{.Name}}, error) {
	query := ` + "`" + `
		SELECT {{.AllColumns}}
		FROM {{.Table}}
		LIMIT $1 OFFSET $2
	` + "`" + `

	rows, err := db.QueryContext(ctx, query, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*{{.Name}}
	for rows.Next() {
		var m {{.Name}}
		if err := rows.Scan({{.AllScanArgs "m"}}); err != nil {
			return nil, err
		}
		results = append(results, &m)
	}
	return results, rows.Err()
}
`

func generateQueries(m Model) (string, error) {
	tmpl, err := template.New("gen").Parse(codeTemplate)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, m); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func toSnakeCase(str string) string {
	var b strings.Builder
	for i, r := range str {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteRune('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
