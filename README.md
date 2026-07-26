# query-gen 🚀

[![Go Reference](https://pkg.go.dev/badge/github.com/abiiranathan/query-gen.svg)](https://pkg.go.dev/github.com/abiiranathan/query-gen)
[![Go Report Card](https://goreportcard.com/badge/github.com/abiiranathan/query-gen)](https://goreportcard.com/badge/github.com/abiiranathan/query-gen)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/mit-license)

**query-gen** is a lightning-fast, zero-reflection compile-time Go code generator that inspects your domain model structs and generates type-safe, high-performance CRUD queries and association preloader methods.

### 💡 Why GORM Struct Tags?
`query-gen` explicitly adopts standard GORM struct tags (`gorm:"primaryKey;column:...;default:...;->;<-"`). This design decision enables **seamless drop-in compatibility** with existing Go projects and microservices using GORM. You can introduce `query-gen` to performance-critical hot paths in your applications without altering your core domain models or refactoring struct tags.

---

## ✨ Features

- **⚡ Zero Reflection at Runtime:** Generates explicit, statically-typed Go code using standard `database/sql`. No runtime query parsing or `reflect` overhead.
- **🗑️ Native Soft Delete Support:** Automatically detects `DeletedAt` fields (or `deleted_at` columns). Soft-deletes records via `UPDATE` by default and filters out deleted rows across all queries automatically.
- **🔗 Smart Association Preloading:** Automatically discovers `HasMany` and `BelongsTo` relationships between models and preloads them in single queries using CTEs or optimized `LEFT JOIN` clauses.
- **🪄 Dynamic & Smart Statements:** `INSERT` statements execute with dynamic column construction to skip uninitialized timestamps or zero-value `default:...` fields, returning all computed columns (`RETURNING *`) back into your model struct.
- **🛡️ Safe Bulk Operations:** Bulk delete methods (`DeleteUsers`, `DeleteOrders`) require at least one query condition to prevent accidental table truncation.
- **📊 Built-in Generic Pagination:** Generic pagination helper (`queries.Paginate`) with total record counts, page count calculations, and navigation flags (`HasNext`, `HasPrev`).
- **🎛️ Rich Expressive Query Options:** Parameterized functional options (`Where`, `Having`, `In`, `NotIn`, `Between`, `Search`, `ILIKE`, `IncludeDeleted`, `HardDelete`, etc.) for safe SQL queries.
- **🏷️ Fine-grained Field Permissions:** Granular field controls (`PermReadOnly`, `PermWriteOnly`, `PermCreateOnly`, `PermUpdateOnly`).

---

## ⚖️ Benefits & Caveats

### Clear Benefits

1. **Maximum Execution Speed:** Bypasses ORM query-building abstractions and dynamic reflection, running at raw `database/sql` driver speed.
2. **Clean & Inspectable Code:** Generated Go files are clean, readable, and easily inspectable in your IDE or step-through debugger.
3. **Transaction Context Aware:** Generated methods accept the generic `DBTX` interface (`*sql.DB` or `*sql.Tx`), allowing effortless participation in database transactions.
4. **Safety Against Data Loss:** Bulk delete operations enforce safety checks against empty condition arguments.

### Caveats

1. **Parameter Placeholder Syntax:** Placeholders strictly use PostgreSQL and SQLite syntax (`$1, $2`). MySQL drivers requiring `?` placeholders are not supported out of the box.
2. **Multi-`HasMany` Joining:** Preloading multiple independent `HasMany` slice relations in a single query uses SQL `LEFT JOIN`s, which can produce Cartesian product row growth for large $M \times N$ relation sets.

---

## 📥 Installation

Install the generator tool using `go install`:

```bash
go install github.com/abiiranathan/query-gen@latest
```

---

## 🚀 Quick Start

### 1. Define Your Models

Define your domain models using standard Go structs:

```go
// models/user.go
package models

import "time"

type User struct {
    ID        int64      `gorm:"primaryKey"`
    Name      string     `gorm:"not null"`
    Email     string     `gorm:"not null"`
    CreatedAt *time.Time
    DeletedAt *time.Time // Enables soft delete support automatically
    Orders    []Order    `gorm:"foreignKey:UserID"`
}

type Order struct {
    ID     int64   `gorm:"primaryKey;column:order_id"`
    UserID int64   `gorm:"not null;column:user_id"`
    Amount float64 `gorm:"not null"`
    User   *User   `gorm:"foreignKey:UserID"`
}
```

### 2. Generate Queries

Run `query-gen` pointing to your models package:

```bash
query-gen -input ./models -out ./queries -pkg queries
```

### 3. Use Generated Queries

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

    // 1. Insert record (computes defaults, updates model fields via RETURNING)
    user := &models.User{
        Name:  "Jane Doe",
        Email: "jane@example.com",
    }
    if err := queries.InsertUser(ctx, db, user); err != nil {
        log.Fatal(err)
    }

    // 2. Fetch record by ID with preloaded orders (excluding soft-deleted)
    fetchedUser, err := queries.GetUserByID(ctx, db, user.ID, queries.Preload(true))
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("User:", fetchedUser.Name)

    // 3. Soft delete record
    if err := queries.DeleteUser(ctx, db, user.ID); err != nil {
        log.Fatal(err)
    }

    // 4. Fetch including soft-deleted records
    deletedUser, err := queries.GetUserByID(ctx, db, user.ID, queries.IncludeDeleted())
    if err == nil {
        fmt.Println("Soft deleted at:", deletedUser.DeletedAt)
    }
}
```

---

## 🗑️ Soft Delete & Bulk Deletion

Models containing a `DeletedAt` field automatically receive soft-delete functionality:

- **Single Record Soft Delete:** `queries.DeleteUser(ctx, db, id)` sets `deleted_at = CURRENT_TIMESTAMP`.
- **Read Query Isolation:** Read queries (`GetByID`, `FetchAll`, `Count`, `Exists`) automatically append `deleted_at IS NULL`.
- **Include Soft-Deleted Records:** Pass `queries.IncludeDeleted()` to read queries.
- **Bulk Soft Delete:** Use `queries.DeleteUsers(ctx, db, queries.Where("status = $1", "inactive"))`.
- **Permanent Hard Delete:** Pass `queries.HardDelete()` to force a SQL `DELETE FROM`:

```go
// Hard delete a specific user permanently
err := queries.DeleteUser(ctx, db, userID, queries.HardDelete())

// Bulk hard delete soft-deleted users older than 30 days
count, err := queries.DeleteUsers(
    ctx, 
    db, 
    queries.Where("deleted_at < $1", thirtyDaysAgo), 
    queries.IncludeDeleted(), 
    queries.HardDelete(),
)
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
    1,  // Page number (1-based)
    10, // Page size
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

| Option                    | Signature / Description                   | Example                                                        |
| :------------------------ | :---------------------------------------- | :------------------------------------------------------------- |
| **`Where`**               | `Where(where string, args ...any)`        | `queries.Where("status = $1", "active")`                       |
| **`Having`**              | `Having(having string, args ...any)`      | `queries.Having("COUNT(*) > $1", 5)`                           |
| **`In`**                  | `In(column string, values ...any)`        | `queries.In("status", "pending", "approved")`                  |
| **`NotIn`**               | `NotIn(column string, values ...any)`     | `queries.NotIn("role", "banned")`                              |
| **`IsNull`**              | `IsNull(column string)`                   | `queries.IsNull("email")`                                      |
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
| **`PreloadAssociations`** | `Preload(preload bool)`                   | `queries.Preload(true)`                                        |
| **`IncludeDeleted`**      | `IncludeDeleted()`                        | `queries.IncludeDeleted()`                                     |
| **`HardDelete`**          | `HardDelete()`                            | `queries.HardDelete()`                                         |

---

## 🛠️ Command-Line Flags

| Flag     | Default             | Description                                                  |
| :------- | :------------------ | :----------------------------------------------------------- |
| `-input` | `./example/models`  | Path to the Go package containing source domain models.      |
| `-out`   | `./example/queries` | Destination directory where generated files will be written. |
| `-pkg`   | `queries`           | Package name declaration for the generated query code.       |

---

## 🔍 Supported GORM Tags

- `gorm:"primaryKey"` or `gorm:"primary_key"` — Marks field as the primary key.
- `gorm:"column:custom_name"` — Overrides default snake_case database column name.
- `gorm:"default:value"` — Defines default values (zero-values are omitted during INSERT).
- `gorm:"not null"` or `gorm:"notnull"` — Marks field as non-nullable.
- `gorm:"null"` — Marks field as nullable.
- `gorm:"-"` — Excludes field from query generation.
- `gorm:"->"` — Read-only field (omitted from INSERT and UPDATE statements).
- `gorm:"<-"` — Write-only field (omitted from SELECT queries).
- `gorm:"<-:create"` — Field is inserted, but never updated.
- `gorm:"<-:update"` — Field is updated, but never inserted.

---

## 🧪 Running Tests

```bash
go test -v ./...
```

---

## 📜 License

Distributed under the **MIT** License. See `LICENSE` for more information.
