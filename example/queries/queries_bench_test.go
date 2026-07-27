package queries_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/abiiranathan/query-gen/example/models"
	"github.com/abiiranathan/query-gen/example/queries"
	"github.com/abiiranathan/query-gen/perms"
)

// setupBenchmarkData creates an in-memory database pre-populated with count users
// and 2 orders per user (totaling count*2 orders) inside a single transaction for speed.
func setupBenchmarkData(b *testing.B, userCount int) *sql.DB {
	b.Helper()

	db := setupTestDB(&testing.T{})

	tx, err := db.Begin()
	if err != nil {
		b.Fatalf("failed to begin seed transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmtUser, err := tx.Prepare("INSERT INTO users (name, email, age) VALUES ($1, $2, $3)")
	if err != nil {
		b.Fatalf("failed to prepare user insert: %v", err)
	}
	defer stmtUser.Close()

	stmtOrder, err := tx.Prepare("INSERT INTO orders (user_id, amount) VALUES ($1, $2)")
	if err != nil {
		b.Fatalf("failed to prepare order insert: %v", err)
	}
	defer stmtOrder.Close()

	for i := 1; i <= userCount; i++ {
		res, err := stmtUser.Exec(fmt.Sprintf("User %d", i), fmt.Sprintf("user%d@benchmark.com", i), "30")
		if err != nil {
			b.Fatalf("failed to insert user %d: %v", i, err)
		}

		userID, err := res.LastInsertId()
		if err != nil {
			b.Fatalf("failed to get user lastInsertId: %v", err)
		}

		// Insert 2 orders per user
		if _, err := stmtOrder.Exec(userID, 49.99); err != nil {
			b.Fatalf("failed to insert order 1 for user %d: %v", userID, err)
		}
		if _, err := stmtOrder.Exec(userID, 120.00); err != nil {
			b.Fatalf("failed to insert order 2 for user %d: %v", userID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		b.Fatalf("failed to commit seed transaction: %v", err)
	}

	return db
}

// BenchmarkPreload_SplitQueries measures the performance of preloading 2,000 users
// and 4,000 associated orders using 2 separate queries (WHERE IN).
func BenchmarkPreload_SplitQueries(b *testing.B) {
	const totalUsers = 20000
	db := setupBenchmarkData(b, totalUsers)
	ctx := context.Background()

	b.ReportAllocs()

	for b.Loop() {
		fetchedUsers, err := queries.FetchAllUsers(ctx, db, queries.Preload(true))
		if err != nil {
			b.Fatalf("FetchAllUsers failed: %v", err)
		}

		if len(fetchedUsers) != totalUsers {
			b.Fatalf("expected %d users, got %d", totalUsers, len(fetchedUsers))
		}
		if len(fetchedUsers[0].Orders) != 2 {
			b.Fatalf("expected 2 preloaded orders, got %d", len(fetchedUsers[0].Orders))
		}
	}
}

func BenchmarkPreload_RawSQLJoinMap(b *testing.B) {
	const totalUsers = 20000
	db := setupBenchmarkData(b, totalUsers)
	ctx := context.Background()

	const query = `
		SELECT 
			u.id, u.name, u.email, u.created_at, u.deleted_at, u.age,
			o.order_id, o.user_id, o.amount
		FROM users u
		LEFT JOIN orders o ON o.user_id = u.id
		WHERE u.deleted_at IS NULL
		ORDER BY u.id ASC
	`

	b.ReportAllocs()
	// Reset timer after seeding 2,000 users and 4,000 orders

	for b.Loop() {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			b.Fatalf("raw join query failed: %v", err)
		}

		itemsMap := make(map[int64]*models.User, totalUsers)
		items := make([]*models.User, 0, totalUsers)
		seenOrders := make(map[int64]map[int64]bool, totalUsers)

		for rows.Next() {
			var (
				u          models.User
				uEmail     sql.NullString
				uCreatedAt sql.NullTime
				uDeletedAt sql.NullTime
				uAge       sql.NullString
				oID        sql.NullInt64
				oUserID    sql.NullInt64
				oAmount    sql.NullFloat64
			)

			if err := rows.Scan(
				&u.ID, &u.Name, &uEmail, &uCreatedAt, &uDeletedAt, &uAge,
				&oID, &oUserID, &oAmount,
			); err != nil {
				rows.Close()
				b.Fatalf("scan row failed: %v", err)
			}

			u.Email = uEmail.String
			u.Age = models.Age(uAge.String)
			if uCreatedAt.Valid {
				u.CreatedAt = perms.Date(uCreatedAt.Time)
			}
			if uDeletedAt.Valid {
				u.DeletedAt = &uDeletedAt.Time
			}

			parent, exists := itemsMap[u.ID]
			if !exists {
				parent = &u
				itemsMap[u.ID] = parent
				items = append(items, parent)
				seenOrders[u.ID] = make(map[int64]bool, 2)
			}

			if oID.Valid {
				orderID := oID.Int64
				if !seenOrders[u.ID][orderID] {
					seenOrders[u.ID][orderID] = true
					parent.Orders = append(parent.Orders, models.Order{
						ID:     orderID,
						UserID: oUserID.Int64,
						Amount: oAmount.Float64,
					})
				}
			}
		}
		rows.Close()

		if err := rows.Err(); err != nil {
			b.Fatalf("rows iteration failed: %v", err)
		}
		if len(items) != totalUsers {
			b.Fatalf("expected %d users, got %d", totalUsers, len(items))
		}
		if len(items[0].Orders) != 2 {
			b.Fatalf("expected 2 preloaded orders, got %d", len(items[0].Orders))
		}
	}
}
