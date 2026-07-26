# query-gen 🚀

[![Go Reference](https://pkg.go.dev/badge/github.com/abiiranathan/query-gen.svg)](https://pkg.go.dev/github.com/abiiranathan/query-gen) [![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/mit-license)

**query-gen** is a fast, zero-reflection compile-time Go code generator that inspects your domain model structs and generates type-safe, high-performance CRUD queries, bulk insertion batchers, and association preloader methods.

### Why GORM Struct Tags?
`query-gen` explicitly adopts standard GORM struct tags (`gorm:"primaryKey;column:...;default:...;->;<-"`). This design decision enables **seamless drop-in compatibility** with existing Go projects and microservices using GORM. You can introduce `query-gen` to performance-critical hot paths in your applications without altering your core domain models or refactoring struct tags.

---

## Comparison & Alternatives

Go developers have several options for database access, ranging from runtime ORMs to SQL-first query compilers. `query-gen` sits in a sweet spot: **Model-First Code Generation**.

| Tool            | Approach           | Schema Source      | Runtime Overhead                              | Model Ownership & Control                                                |
| :-------------- | :----------------- | :----------------- | :-------------------------------------------- | :----------------------------------------------------------------------- |
| **`query-gen`** | **Model-First**    | Go Structs         | ⚡ Zero (Compile-time code gen)                | **Full Control** (Your structs, your tags, your methods)                 |
| **`sqlc`**      | **SQL-First**      | Raw `.sql` Queries | ⚡ Zero (Compile-time code gen)                | **Low** (Generated structs; custom tags/methods require heavy overrides) |
| **`GORM`**      | **Model-First**    | Go Structs         | 🐢 Moderate (Runtime reflection & dynamic SQL) | **Full Control**                                                         |
| **`SQLBoiler`** | **Database-First** | Live DB Connection | ⚡ Low                                         | **Low** (Generated models tied to live schema)                           |
| **`sqlx`**      | **Manual SQL**     | Hand-written SQL   | 🐢 Minor (Runtime struct reflection)           | **Full Control**                                                         |

### 🛠️ `query-gen` vs `sqlc` (Model-First vs. SQL-First)
`sqlc` is a fantastic tool, but it represents the **exact opposite workflow**:
- **SQL-First (`sqlc`):** You write raw `.sql` files, and `sqlc` generates Go models and query functions for you. However, because `sqlc` owns the generated struct files, customizing struct tags (e.g., `json`, `validate`, `xml`), embedding third-party types, or attaching domain receiver methods requires complex configuration overrides, wrapper types, or custom plugins.
- **Model-First (`query-gen`):** You own and define your domain models in plain Go structs. You can add any tags, methods, or helper functions directly to your structs without fighting a generator or maintaining external `.sql` files for basic CRUD operations. `query-gen` inspects your structs and generates type-safe CRUD operations using `database/sql`.

### ⚡ `query-gen` vs `GORM` (Compile-Time vs. Runtime)
`GORM` builds SQL dynamically at runtime using `reflect` and internal metadata maps. `query-gen` uses the same familiar `gorm:"..."` tags but parses them **once at compile time**. The output is raw, static Go code (`row.Scan(...)`), eliminating reflection overhead and producing execution speeds identical to hand-written `database/sql` code.

### 🔌 `query-gen` vs `SQLBoiler` (Code-First vs. DB-First)
`SQLBoiler` requires a running database connection to inspect tables and generate code. `query-gen` operates purely on Go source AST code (`go/ast`), allowing code generation in CI/CD pipelines without needing a running database instance.

---

## ✨ Features

- **⚡ Zero Reflection at Runtime:** Generates explicit, statically-typed Go code using standard `database/sql`. No runtime query parsing or `reflect` overhead.
- **📦 High-Performance Parameter-Bounded Bulk Inserts:** Efficiently bulk insert thousands of records (`InsertUsers`) with automatic parameter-limit chunking (max 999 parameters) and `RETURNING` primary key population.
- **🗑️ Native Soft Delete Support:** Automatically detects `DeletedAt` fields (or `deleted_at` columns). Soft-deletes records via `UPDATE` by default and filters out deleted rows across all queries automatically.
- **🔗 Smart Association Preloading:** Automatically discovers `HasMany` and `BelongsTo` relationships between models and preloads them using single `LEFT JOIN` CTEs or optimized batch `IN` queries.
- **🔑 Composite Primary Key Support:** Fully supports models with single or composite primary keys across all generated lookup, update, and deletion queries.
- **📜 Automatic SQL Schema DDL Generation:** Optional generation of SQL schema files (`-schema` flag) targeting **PostgreSQL** or **SQLite3** dialects.
- **🪄 Dynamic & Smart Statements:** Single `INSERT` statements execute with dynamic column construction to skip uninitialized timestamps or zero-value `default:...` fields, returning all computed columns (`RETURNING *`) back into your model struct.
- **🛡️ Safe Bulk Operations:** Bulk delete methods (`DeleteUsers`, `DeleteOrders`) require at least one query condition to prevent accidental table truncation.
- **📊 Built-in Generic Pagination:** Generic pagination helper (`queries.Paginate`) with total record counts, page count calculations, and navigation flags (`HasNext`, `HasPrev`).
- **🎛️ Rich Expressive Query Options:** Parameterized functional options (`Where`, `Having`, `In`, `NotIn`, `Between`, `Search`, `ILIKE`, `IncludeDeleted`, `HardDelete`, etc.) for safe SQL queries.

---

## ⚖️ Benefits & Caveats

### Clear Benefits

1. **Maximum Execution Speed:** Bypasses ORM query-building abstractions and dynamic reflection, running at raw `database/sql` driver speed.
2. **Clean & Inspectable Code:** Generated Go files are clean, readable, and easily inspectable in your IDE or step-through debugger.
3. **Transaction Context Aware:** Generated methods accept the generic `DBTX` interface (`*sql.DB` or `*sql.Tx`), allowing effortless participation in database transactions.
4. **Safety Against Data Loss:** Bulk delete operations enforce safety checks against empty condition arguments.
5. **Respect for sql.Scanner and driver.Valuer** interfaces for your custom types.

### Caveats

1. **Parameter Placeholder Syntax:** Placeholders strictly use PostgreSQL and SQLite syntax (`$1, $2`). MySQL drivers requiring `?` placeholders are not supported out of the box.
2. There may be limited reflection if scanning a potentially NULL value especially *time.Time. This avoid common time.Time scanning errors when src is nil.
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
    Email     string     `gorm:"not null;unique"`
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

### 2. Generate Queries & SQL Schema

Run `query-gen` pointing to your models package:

```bash
query-gen -input ./models -out ./queries -pkg queries -schema ./schema.sql -dbtype postgres
```

### 3. Use Generated Queries

```go
package main

import (
    "context"
    "database/sql"
    "fmt"
    "log"

    _ "modernc.org/sqlite" // Replace with your own driver
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

    // 1. Single Insert (populates generated ID via RETURNING)
    user := &models.User{
        Name:  "Jane Doe",
        Email: "jane@example.com",
    }
    if err := queries.InsertUser(ctx, db, user); err != nil {
        log.Fatal(err)
    }
    fmt.Println("Inserted User ID:", user.ID)

    // 2. Bulk Insert 1,000 Users (automatically batched in parameter-safe chunks)
    bulkUsers := make([]*models.User, 1000)
    for i := 0; i < 1000; i++ {
        bulkUsers[i] = &models.User{
            Name:  fmt.Sprintf("User %d", i+1),
            Email: fmt.Sprintf("user%d@example.com", i+1),
        }
    }
    if err := queries.InsertUsers(ctx, db, bulkUsers); err != nil {
        log.Fatal(err)
    }
    fmt.Println("Bulk inserted 1,000 users! First ID:", bulkUsers[0].ID, "Last ID:", bulkUsers[999].ID)

    // 3. Fetch record by ID with preloaded orders
    fetchedUser, err := queries.GetUserByID(ctx, db, user.ID, queries.Preload(true))
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println("Fetched User:", fetchedUser.Name)

    // 4. Soft delete record
    if err := queries.DeleteUser(ctx, db, user.ID); err != nil {
        log.Fatal(err)
    }
}
```

4. PostgresQL example
```go
package main

import (
    "context"
    "database/sql"
    "fmt"
    "log"
    "os"
    "time"

    "github.com/abiiranathan/dbtypes"
    "mpapp/queries"
    "mpapp/models"
    _ "github.com/jackc/pgx/v5/stdlib" // Registers "pgx" driver
    "github.com/joho/godotenv"
)

func main() {
    if err := godotenv.Load(); err != nil {
        log.Println("Notice: No .env file found, reading environment from system")
    }

    dsn := os.Getenv("DSN")
    if dsn == "" {
        log.Fatal("Fatal: DSN environment variable is not set")
    }

    // Open database connection using standard database/sql and pgx driver.
    db, err := sql.Open("pgx", dsn)
    if err != nil {
        log.Fatalf("Unable to initialize database driver: %v", err)
    }
    defer db.Close()

    // Configure pool parameters on *sql.DB.
    db.SetMaxOpenConns(25)
    db.SetMaxIdleConns(5)
    db.SetConnMaxLifetime(30 * time.Minute)

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    // Ping to verify connectivity.
    if err := db.PingContext(ctx); err != nil {
        log.Fatalf("Database ping failed: %v", err)
    }

    fmt.Println("Successfully connected!")

    // Using transactions
    // Transaction sets app.current_tenant_id for Postgres RLS policies.
    tx, err := db.BeginTx(ctx, nil)
    if err != nil {
        log.Fatalln(err)
    }
    defer tx.Rollback()

    deposit := &models.Deposit{
        Date:          dbtypes.Today(),
        Bank:          "DFCU",
        AccountNumber: "A1023784994",
        Amount:        5000,
        Issuer:        "Issuer",
        Depositor:     "Depositor",
        CreatedAt:     time.Now(),
    }
    err = queries.InsertDeposit(ctx, tx, deposit)

    if err != nil {
        log.Fatalln(err)
    }

    // Fetch all deposits
    deposits, err := queries.FetchAllDeposits(ctx, tx)
    if err != nil {
        log.Fatalln(err)
    }

    for _, d := range deposits {
        fmt.Printf("New deposit: Hosp ID: %d - %s: %d\n", d.HospitalID, d.AccountNumber, d.Amount)
        fmt.Printf("%+v\n", deposit)
    }

    // Fetch deposit back
    fmt.Println("========== Fetch deposit back =================")
    deposit, err = queries.GetDepositByID(ctx, tx, deposit.ID, queries.Preload(true))
    if err != nil {
        log.Fatalln(err)
    }
    fmt.Printf("%+v\n", deposit)

    err := queries.DeleteDeposit(ctx, tx,  deposit.ID)
    if err != nil {
        log.Fatalln(err)
    }

    err = tx.Commit()
    if err != nil {
     log.Fatalf("commit error: %v\n", err)
    }
}

```

---

## 📖 Generated Methods Reference

For every exported model struct (e.g. `User`), `query-gen` creates a complete set of statically-typed functions using the model name (`User`) and its plural (`Users`):

| Method Signature                           | Return Type               | Description                                                                                                                  |
| :----------------------------------------- | :------------------------ | :--------------------------------------------------------------------------------------------------------------------------- |
| **`InsertUser(ctx, db, m)`**               | `error`                   | Inserts a single record. Omits zero-value default fields and scans generated primary keys / defaults back into `m`.          |
| **`InsertUsers(ctx, db, models)`**         | `error`                   | Bulk inserts a slice of structs in parameter-bounded batches. Automatically updates all structs with generated primary keys. |
| **`GetUserByID(ctx, db, id, opts...)`**    | `(*models.User, error)`   | Retrieves a single record by primary key (supports `Preload(true)` and `IncludeDeleted()`).                                  |
| **`ExistsUserByID(ctx, db, id, opts...)`** | `(bool, error)`           | Reports whether a record matching the primary key exists.                                                                    |
| **`CountUsers(ctx, db, opts...)`**         | `(int64, error)`          | Returns total count of matching rows based on optional filter criteria.                                                      |
| **`FetchAllUsers(ctx, db, opts...)`**      | `([]*models.User, error)` | Retrieves a filtered, ordered, or paginated slice of records (supports `Preload(true)`).                                     |
| **`UpdateUser(ctx, db, m)`**               | `error`                   | Updates all non-primary key fields for the given model instance matching its primary key.                                    |
| **`DeleteUser(ctx, db, id, opts...)`**     | `error`                   | Deletes a record by primary key (soft delete if `DeletedAt` exists; permanent delete if `HardDelete()` is passed).           |
| **`DeleteUsers(ctx, db, opts...)`**        | `(int64, error)`          | Deletes multiple records matching options. Requires a `Where` clause to prevent accidental full table truncation.            |

---

## 📦 Bulk Insertion Deep Dive

The generated bulk insertion method (`InsertUsers`, `InsertOrders`, etc.) is built for high-throughput data loading:

- **Automatic Parameter Batching:** Calculates the exact max batch size per model based on column count (`999 / numColumns`). For example, a model with 3 writable columns processes 333 rows per query batch, keeping parameter counts safely under SQLite (`999`) and PostgreSQL parameter limits.
- **Zero Allocation Placeholder Lookup:** Uses pre-calculated parameter string caching (`$1`, `$2`, ..., `$999`) to build query strings without string allocations.
- **In-Place ID & Default Population:** Leverages SQL `RETURNING *` and scans generated primary keys, timestamps, and defaults back into your input slice pointers.

```go
var newUsers []*models.User
// ... populate slice with 5,000 items ...

// Executes in multiple parameter-safe database queries automatically
err := queries.InsertUsers(ctx, db, newUsers)
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

> You must pass 2 callback functions. One for counting the records and another for actually fetching the right span based on page and page size.

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

| Option               | Signature / Description                   | Example                                                        |
| :------------------- | :---------------------------------------- | :------------------------------------------------------------- |
| **`Where`**          | `Where(where string, args ...any)`        | `queries.Where("status = $1", "active")`                       |
| **`Having`**         | `Having(having string, args ...any)`      | `queries.Having("COUNT(*) > $1", 5)`                           |
| **`In`**             | `In(column string, values ...any)`        | `queries.In("status", "pending", "approved")`                  |
| **`NotIn`**          | `NotIn(column string, values ...any)`     | `queries.NotIn("role", "banned")`                              |
| **`IsNull`**         | `IsNull(column string)`                   | `queries.IsNull("email")`                                      |
| **`IsNotNull`**      | `IsNotNull(column string)`                | `queries.IsNotNull("email")`                                   |
| **`Between`**        | `Between(column string, min, max any)`    | `queries.Between("age", 18, 65)`                               |
| **`ILIKE`**          | `ILIKE(column, value string)`             | `queries.ILIKE("name", "john")`                                |
| **`Search`**         | `Search(query string, columns ...string)` | `queries.Search("term", "name", "email")`                      |
| **`Gt` / `Gte`**     | `Gt(col, val)` / `Gte(col, val)`          | `queries.Gte("amount", 100.0)`                                 |
| **`Lt` / `Lte`**     | `Lt(col, val)` / `Lte(col, val)`          | `queries.Lt("stock", 10)`                                      |
| **`DateRange`**      | `DateRange(col, start, end string)`       | `queries.DateRange("created_at", "2024-01-01", "2024-12-31")`  |
| **`MonthRange`**     | `MonthRange(col, start, end string)`      | `queries.MonthRange("created_at", "2024-01-01", "2024-06-30")` |
| **`YearRange`**      | `YearRange(col, start, end string)`       | `queries.YearRange("created_at", "2020-01-01", "2024-12-31")`  |
| **`OrderBy`**        | `OrderBy(orderBy string)`                 | `queries.OrderBy("created_at DESC")`                           |
| **`GroupBy`**        | `GroupBy(groupBy string)`                 | `queries.GroupBy("user_id")`                                   |
| **`Limit`**          | `Limit(limit int)`                        | `queries.Limit(25)`                                            |
| **`Offset`**         | `Offset(offset int)`                      | `queries.Offset(50)`                                           |
| **`Preload`**        | `Preload(preload bool)`                   | `queries.Preload(true)`                                        |
| **`IncludeDeleted`** | `IncludeDeleted()`                        | `queries.IncludeDeleted()`                                     |
| **`HardDelete`**     | `HardDelete()`                            | `queries.HardDelete()`                                         |

---

## 🛠️ Command-Line Flags

| Flag      | Default             | Description                                                                     |
| :-------- | :------------------ | :------------------------------------------------------------------------------ |
| `-input`  | `./example/models`  | Path to the Go package directory containing source models.                      |
| `-out`    | `./example/queries` | Destination directory where generated Go files will be written.                 |
| `-pkg`    | `queries`           | Package name declaration for the generated query code.                          |
| `-schema` | `""`                | Optional path to output a generated SQL schema DDL file (e.g., `./schema.sql`). |
| `-dbtype` | `postgres`          | Target database dialect for `-schema`: `postgres` or `sqlite3`.                 |

---

## 🔍 Supported GORM Tags & Customizations
- `gorm:"primaryKey"` or `gorm:"primary_key"` — Marks field as primary key (supports composite primary keys).
- `gorm:"column:custom_name"` — Overrides default snake_case database column name.
- `gorm:"default:value"` — Defines default values (zero-values are omitted during single `INSERT`).
- `gorm:"not null"` or `gorm:"notnull"` — Marks field as non-nullable.
- `gorm:"null"` — Marks field as nullable.
- `gorm:"-"` — Excludes field completely from query generation.
- `gorm:"->"` — Read-only field (omitted from INSERT and UPDATE statements).
- `gorm:"->:false"` — Disables read access; omitted from SELECT statements (write-only).
- `gorm:"<-"` — Write-only field (omitted from SELECT queries).
- `gorm:"<-:false"` — Disables write access; omitted from INSERT and UPDATE statements (read-only).
- `gorm:"<-:create"` — Field is inserted, but never updated.
- `gorm:"<-:update"` — Field is updated, but never inserted.

### Custom Table Names & Data Types

You can override inferred table names or custom column SQL types by implementing `TableName()` or `DataType()` / `GormDataType()` methods on your structs:

```go
func (User) TableName() string {
    return "custom_users_table"
}

func (User) GormDataType() string {
    return "jsonb"
}
```

---

## 🧪 Running Tests

```bash
go test -v ./...
```

---

## 📜 License

Distributed under the **MIT** License. See `LICENSE` for more information.
