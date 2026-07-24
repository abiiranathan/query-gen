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
- **🏷️ GORM-Compatible Tag Parsing:** Respects standard GORM struct tags (`gorm:"primaryKey;column:...;not null;->;<-"`) out of the box, making it drop-in compatible with existing model definitions.
- **🛡️ Granular Field Permissions:** Fine-grained control over SELECT, INSERT, and UPDATE visibility via read/write field tags (`PermReadOnly`, `PermWriteOnly`, `PermCreateOnly`, `PermUpdateOnly`).
- **📦 Comprehensive Runtime Helpers:** Comes bundled with flexible `QueryOption` functional options (`WithWhere`, `WithOrderBy`, `WithGroupBy`, `WithLimit`, `WithOffset`, `WithPreloadAssociations`) for dynamic filtering and pagination.
- **🛠️ Robust Null-Safety:** Seamlessly handles SQL `NULL` values through generic nullable scanners that convert seamlessly into standard Go primitives or pointers.

---

## 📥 Installation

Install the tool using `go install`:

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
    user := &models.User{
        Name:  "Jane Doe",
        Email: stringPtr("jane@example.com"),
    }
    if err := queries.InsertUser(ctx, db, user); err != nil {
        log.Fatal(err)
    }

    // 2. Fetch record by ID with associations preloaded
    fetchedUser, err := queries.GetUserByID(ctx, db, user.ID, queries.WithPreloadAssociations(true))
    if err != nil {
        log.Fatal(err)
    }

    log.Printf("Fetched User: %+v with %d orders\n", fetchedUser, len(fetchedUser.Orders))

    // 3. Fetch with custom filters and pagination
    activeUsers, err := queries.FetchAllUsers(ctx, db,
        queries.WithWhere("name LIKE $1", "Jane%"),
        queries.WithOrderBy("created_at DESC"),
        queries.WithLimit(10),
    )
    if err != nil {
        log.Fatal(err)
    }
    
    _ = activeUsers
}

func stringPtr(s string) *string { return &s }
```

---

## 🎛️ Command-Line Options

| Flag     | Default             | Description                                                  |
| :------- | :------------------ | :----------------------------------------------------------- |
| `-input` | `./example/models`  | Path to the Go package containing source models.             |
| `-out`   | `./example/queries` | Destination directory where generated files will be written. |
| `-pkg`   | `queries`           | Package name declaration for the generated query files.      |

---

## 🔍 Supported GORM Tags

`query-gen` parses structural annotations using standard tag conventions:

- `gorm:"primaryKey"` or `gorm:"primary_key"` — Marks the field as the primary key.
- `gorm:"column:custom_name"` — Overrides the default snake_case database column name.
- `gorm:"not null"` or `gorm:"notnull"` — Marks the field as non-nullable.
- `gorm:"null"` — Marks the field as nullable.
- `gorm:"-"` — Ignores the field entirely during code generation.
- `gorm:"->"` — Read-only field (omitted from INSERT and UPDATE statements).
- `gorm:"<-"` — Write-only field (omitted from SELECT queries).
- `gorm:"<-:create"` — Field is inserted, but never updated.
- `gorm:"<-:update"` — Field is updated, but never inserted.

---

## 🧪 Running Tests

Validate your generated setup against your database dialect:

```bash
go test -v ./...
```

---

## 🤝 Contributing

Contributions, issues, and feature requests are welcome! Feel free to check the [issues page](https://github.com/abiiranathan/query-gen/issues).

---

## 📜 License

Distributed under the **MIT** License. See `LICENSE` for more information.