# query-gen 🚀

[![Go Reference](https://pkg.go.dev/badge/github.com/abiiranathan/query-gen.svg)](https://pkg.go.dev/github.com/abiiranathan/query-gen)
[![Go Report Card](https://goreportcard.com/badge/github.com/abiiranathan/query-gen)](https://goreportcard.com/badge/github.com/abiiranathan/query-gen)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/mit-license)

**query-gen** is a lightning-fast, zero-reflection compile-time Go code generator that inspects your domain model structs and generates type-safe, high-performance CRUD queries and association preloader methods. 

Inspired by the ergonomics of modern ORMs like GORM, but operating entirely via AST (Abstract Syntax Tree) parsing at build/generation time, `query-gen` delivers raw `database/sql` speed with zero runtime reflection overhead.

---

## ✨ Features

- **⚡ Zero Reflection at Runtime:** Generates explicit, statically-typed Go code using `database/sql`. No runtime query building or struct reflection penalties.
- **🔗 Smart Association Preloading:** Automatically discovers `HasMany` and `BelongsTo` relationships between models and preloads them in optimized single queries using CTEs (Common Table Expressions) or efficient JOINs.
- **📊 Built-in Pagination:** First-class generic pagination support (`queries.Paginate`) with total record counts, page count calculations, and navigation flags.
- **🎛️ Rich Expressive Query Options:** Functional options (`Where`, `Having`, `In`, `NotIn`, `IsNull`, `Between`, `Search`, `ILIKE`, `DateRange`, etc.) for building clean, parameterized SQL queries safely.
- **🏷️ GORM-Compatible Tag Parsing:** Respects standard GORM struct tags (`gorm:"primaryKey;column:...;not null;->;<-"`) out of the box.
- **🛡️ Granular Field Permissions:** Fine-grained control over SELECT, INSERT, and UPDATE visibility via read/write field tags (`PermReadOnly`, `PermWriteOnly`, `PermCreateOnly`, `PermUpdateOnly`).
- **🛠️ Robust Null-Safety:** Seamlessly handles SQL `NULL` values through generic nullable scanners that convert seamlessly into standard Go primitives or pointers.

---

## 📥 Installation

Install the generator tool using `go install`:

```bash
go install github.com/abiiranathan/query-gen@latest
```

---

## 🚀 Quick Start

### 1. Define Your Models

Create your domain models with standard Go structs and GORM tags:

```go
// models/user.go
package models

import "time"

type User struct {
    ID        int64     `gorm:"primaryKey"`
    Name      string    `gorm:"not null"`
    Email     *string   // Nullable string
    Orders    []Order   `gorm:"foreignKey:UserID"`
    CreatedAt time.Time
}

type Order struct {
    ID     int64   `gorm:"primaryKey;column:order_id"`
    UserID int64   `gorm:"not null;column:user_id"`
    Amount float64 `gorm:"not null"`
    User   *User   `gorm:"foreignKey:UserID"`
}
```

### 2. Generate Queries

Run the generator pointing it to your models package:

```bash
query-gen -input ./models -out ./queries -pkg queries
```

### 3. Use the Generated Queries

Enjoy fully type-safe, context-aware database interactions:

```go
package main

import (
    "context"
    "database/sql"
    "fmt"
    "log"

    _ "modernc.org/sqlite"
    "myapp/models"
    "myapp/queries"
)

func main() {
    ctx := context.Background()
    db, err := sql.Open("sqlite", "app.db")
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()

    // 1. Insert a new record
    email := "jane@example.com"
    user := &models.User{
        Name:  "Jane Doe",
        Email: &email,
    }
    if err := queries.InsertUser(ctx, db, user); err != nil {
        log.Fatal(err)
    }

    // 2. Fetch record by ID with preloaded associations
    fetchedUser, err := queries.GetUserByID(ctx, db, user.ID, queries.PreloadAssociations(true))
    if err != nil {
        log.Fatal(err)
    }

    log.Printf("Fetched User: %s with %d orders\n", fetchedUser.Name, len(fetchedUser.Orders))

    // 3. Fetch with custom filters
    activeUsers, err := queries.FetchAllUsers(ctx, db,
        queries.ILIKE("name", "Jane"),
        queries.IsNotNull("email"),
        queries.OrderBy("created_at DESC"),
        queries.Limit(10),
    )
    if err != nil {
        log.Fatal(err)
    }
    
    fmt.Printf("Found %d matching users\n", len(activeUsers))
}
```

---

## 📄 Paginated Queries

Use `queries.Paginate` to fetch paginated datasets along with metadata (`TotalPages`, `HasNext`, `HasPrev`, `Count`):

```go
pageResult, err := queries.Paginate(
    ctx,
    db,
    queries.CountUsers,
    queries.FetchAllUsers,
    1,  // page number (1-based)
    10, // page size
    queries.ILIKE("name", "Jane"),
    queries.OrderBy("id ASC"),
)
if err != nil {
    log.Fatal(err)
}

fmt.Printf("Page %d of %d (Total: %d)\n", pageResult.Page, pageResult.TotalPages, pageResult.Count)
for _, u := range pageResult.Results {
    fmt.Println("-", u.Name)
}
```

### `PaginationResult[T]` JSON Structure

```go
type PaginationResult[T any] struct {
    Page       int   `json:"page"`
    PageSize   int   `json:"page_size"`
    TotalPages int64 `json:"total_pages"`
    Count      int64 `json:"count"`
    HasNext    bool  `json:"has_next"`
    HasPrev    bool  `json:"has_prev"`
    Results    []T   `json:"results"`
}
```

---

## 🎛️ Query Options Reference

All option builders are passed directly into generated fetchers or `queries.Paginate`:

| Option                    | Signature / Description                   | Example                                                        |
| :------------------------ | :---------------------------------------- | :------------------------------------------------------------- |
| **`Where`**               | `Where(where string, args ...any)`        | `queries.Where("status = $1", "active")`                       |
| **`Having`**              | `Having(having string, args ...any)`      | `queries.Having("COUNT(*) > $1", 5)`                           |
| **`In`**                  | `In(column string, values ...any)`        | `queries.In("status", "pending", "approved")`                  |
| **`NotIn`**               | `NotIn(column string, values ...any)`     | `queries.NotIn("role", "banned")`                              |
| **`IsNull`**              | `IsNull(column string)`                   | `queries.IsNull("deleted_at")`                                 |
| **`IsNotNull`**           | `IsNotNull(column string)`                | `queries.IsNotNull("email")`                                   |
| **`Between`**             | `Between(column string, min, max any)`    | `queries.Between("age", 18, 65)`                               |
| **`ILIKE`**               | `ILIKE(column, value string)`             | `queries.ILIKE("name", "john")`                                |
| **`Search`**              | `Search(query string, columns ...string)` | `queries.Search("term", "name", "email")`                      |
| **`Gt` / `Gte`**          | `Gt(col, val)` / `Gte(col, val)`          | `queries.Gte("amount", 100.0)`                                 |
| **`Lt` / `Lte`**          | `Lt(col, val)` / `Lte(col, val)`          | `queries.Lt("stock", 10)`                                      |
| **`DateRange`**           | `DateRange(col, start, end string)`       | `queries.DateRange("created_at", "2024-01-01", "2024-12-31")`  |
| **`MonthRange`**          | `MonthRange(col, start, end string)`      | `queries.MonthRange("created_at", "2024-01-01", "2024-06-30")` |
| **`YearRange`**           | `YearRange(col, start, end string)`       | `queries.YearRange("created_at", "2020-01-01", "2024-12-31")`  |
| **`OrderBy`**             | `OrderBy(orderBy string)`                 | `queries.OrderBy("created_at DESC")`                           |
| **`GroupBy`**             | `GroupBy(groupBy string)`                 | `queries.GroupBy("user_id")`                                   |
| **`Limit`**               | `Limit(limit int)`                        | `queries.Limit(25)`                                            |
| **`Offset`**              | `Offset(offset int)`                      | `queries.Offset(50)`                                           |
| **`PreloadAssociations`** | `PreloadAssociations(preload bool)`       | `queries.PreloadAssociations(true)`                            |

---

## 🛠️ Command-Line Options

| Flag     | Default             | Description                                                  |
| :------- | :------------------ | :----------------------------------------------------------- |
| `-input` | `./example/models`  | Path to the Go package containing source domain models.      |
| `-out`   | `./example/queries` | Destination directory where generated files will be written. |
| `-pkg`   | `queries`           | Package name declaration for the generated query code.       |

---

## 🔍 Supported GORM Tags

`query-gen` parses structural annotations using standard tag conventions:

- `gorm:"primaryKey"` or `gorm:"primary_key"` — Marks the field as the primary key.
- `gorm:"column:custom_name"` — Overrides default snake_case database column name.
- `gorm:"not null"` or `gorm:"notnull"` — Marks the field as non-nullable.
- `gorm:"null"` — Marks the field as nullable.
- `gorm:"-"` — Ignores the field entirely during code generation.
- `gorm:"->"` — Read-only field (omitted from INSERT and UPDATE statements).
- `gorm:"<-"` — Write-only field (omitted from SELECT queries).
- `gorm:"<-:create"` — Field is inserted, but never updated.
- `gorm:"<-:update"` — Field is updated, but never inserted.

---

## 🧪 Running Tests

Validate your generated query setup against your target database driver:

```bash
go test -v ./...
```

---

## 🤝 Contributing

Contributions, issues, and feature requests are welcome! Feel free to check the [issues page](https://github.com/abiiranathan/query-gen/issues).

---

## 📜 License

Distributed under the **MIT** License. See `LICENSE` for more information.
