package queries_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/abiiranathan/query-gen/example/models"
	"github.com/abiiranathan/query-gen/example/queries"
	_ "modernc.org/sqlite"
)

// setupTestDB creates an in-memory SQLite database matching your exact GORM tags.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", "file::memory:?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		email TEXT,
		created_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS orders (
		order_id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		amount REAL NOT NULL,
		FOREIGN KEY (user_id) REFERENCES users(id)
	);
	`

	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("failed to execute test schema: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
	})

	return db
}

func TestInsertAndGetByID(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	user := &models.User{
		Name:      "Alice",
		Email:     "alice@example.com",
		CreatedAt: now,
	}

	// 1. Test Insert
	if err := queries.InsertUser(ctx, db, user); err != nil {
		t.Fatalf("InsertUser failed: %v", err)
	}
	if user.ID == 0 {
		t.Fatalf("expected non-zero user ID after insert")
	}

	// 2. Test GetByID
	fetched, err := queries.GetUserByID(ctx, db, user.ID)
	if err != nil {
		t.Fatalf("GetUserByID failed: %v", err)
	}

	if fetched.Name != user.Name || fetched.Email != user.Email {
		t.Errorf("mismatch fetched user: got %+v, want %+v", fetched, user)
	}
}

func TestExistsAndCount(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Seed data
	users := []*models.User{
		{Name: "User 1", Email: "u1@test.com"},
		{Name: "User 2", Email: "u2@test.com"},
		{Name: "User 3", Email: "u3@test.com"},
	}
	for _, u := range users {
		if err := queries.InsertUser(ctx, db, u); err != nil {
			t.Fatalf("failed to insert seed user: %v", err)
		}
	}

	// 1. Test ExistsByID
	exists, err := queries.ExistsUserByID(ctx, db, users[0].ID)
	if err != nil || !exists {
		t.Errorf("ExistsUserByID returned (%v, %v), want (true, nil)", exists, err)
	}

	notExists, err := queries.ExistsUserByID(ctx, db, 99999)
	if err != nil || notExists {
		t.Errorf("ExistsUserByID returned (%v, %v), want (false, nil)", notExists, err)
	}

	// 2. Test Count without options
	totalCount, err := queries.CountUsers(ctx, db)
	if err != nil || totalCount != 3 {
		t.Errorf("CountUsers total = %d, want 3 (err: %v)", totalCount, err)
	}

	// 3. Test Count with QueryOptions (WithWhere)
	filteredCount, err := queries.CountUsers(ctx, db, queries.WithWhere("email = $1", "u1@test.com"))
	if err != nil || filteredCount != 1 {
		t.Errorf("CountUsers with filter = %d, want 1 (err: %v)", filteredCount, err)
	}
}

func TestFetchAllWithOptions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Seed 5 users
	names := []string{"Charlie", "Bob", "Alice", "David", "Eve"}
	for _, name := range names {
		_ = queries.InsertUser(ctx, db, &models.User{Name: name, Email: name + "@test.com"})
	}

	t.Run("Where and OrderBy", func(t *testing.T) {
		res, err := queries.FetchAllUsers(ctx, db,
			queries.WithWhere("email LIKE $1", "%test.com"),
			queries.WithOrderBy("name ASC"),
		)
		if err != nil {
			t.Fatalf("FetchAllUsers failed: %v", err)
		}
		if len(res) != 5 {
			t.Fatalf("got %d users, want 5", len(res))
		}
		if res[0].Name != "Alice" || res[4].Name != "Eve" {
			t.Errorf("incorrect ordering: first=%s, last=%s", res[0].Name, res[4].Name)
		}
	})

	t.Run("Limit and Offset Pagination", func(t *testing.T) {
		res, err := queries.FetchAllUsers(ctx, db,
			queries.WithOrderBy("name ASC"),
			queries.WithLimit(2),
			queries.WithOffset(1),
		)
		if err != nil {
			t.Fatalf("FetchAllUsers with limit/offset failed: %v", err)
		}
		if len(res) != 2 {
			t.Fatalf("got %d users, want 2", len(res))
		}

		// Alphabetical: Alice, Bob, Charlie, David, Eve
		// Offset 1, Limit 2 => Bob, Charlie
		if res[0].Name != "Bob" || res[1].Name != "Charlie" {
			t.Errorf("unexpected paginated result: got [%s, %s], want [Bob, Charlie]", res[0].Name, res[1].Name)
		}
	})
}

func TestHasManyRelation(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Seed Parent
	user := &models.User{Name: "Order Owner", Email: "owner@test.com"}
	if err := queries.InsertUser(ctx, db, user); err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}

	// Seed Children
	orders := []*models.Order{
		{UserID: user.ID, Amount: 100.50},
		{UserID: user.ID, Amount: 49.99},
	}
	for _, o := range orders {
		if err := queries.InsertOrder(ctx, db, o); err != nil {
			t.Fatalf("failed to insert order: %v", err)
		}
	}

	t.Run("GetUserWithOrdersByID", func(t *testing.T) {
		fetchedUser, err := queries.GetUserWithOrdersByID(ctx, db, user.ID)
		if err != nil {
			t.Fatalf("GetUserWithOrdersByID failed: %v", err)
		}
		if len(fetchedUser.Orders) != 2 {
			t.Fatalf("got %d orders, want 2", len(fetchedUser.Orders))
		}
	})

	t.Run("FetchAllUsersWithOrders", func(t *testing.T) {
		usersWithOrders, err := queries.FetchAllUsersWithOrders(ctx, db,
			queries.WithWhere("p.id = $1", user.ID),
		)
		if err != nil {
			t.Fatalf("FetchAllUsersWithOrders failed: %v", err)
		}
		if len(usersWithOrders) != 1 {
			t.Fatalf("got %d parent users, want 1", len(usersWithOrders))
		}
		if len(usersWithOrders[0].Orders) != 2 {
			t.Errorf("got %d joined orders, want 2", len(usersWithOrders[0].Orders))
		}
	})
}

func TestBelongsToRelation(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Seed Parent
	user := &models.User{Name: "Parent User", Email: "parent@test.com"}
	if err := queries.InsertUser(ctx, db, user); err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}

	// Seed Child (Note: Order primary key maps to column 'order_id')
	order := &models.Order{UserID: user.ID, Amount: 250.00}
	if err := queries.InsertOrder(ctx, db, order); err != nil {
		t.Fatalf("failed to insert order: %v", err)
	}
	if order.ID == 0 {
		t.Fatalf("expected non-zero Order.ID (order_id) after insert")
	}

	t.Run("GetOrdersWithUserByID", func(t *testing.T) {
		fetchedOrder, err := queries.GetOrdersWithUserByID(ctx, db, order.ID)
		if err != nil {
			t.Fatalf("GetOrdersWithUserByID failed: %v", err)
		}
		if fetchedOrder.User == nil {
			t.Fatalf("expected order.User to be populated, got nil")
		}
		if fetchedOrder.User.Name != user.Name {
			t.Errorf("got user name %q, want %q", fetchedOrder.User.Name, user.Name)
		}
	})

	t.Run("FetchAllOrdersWithUser", func(t *testing.T) {
		ordersWithUser, err := queries.FetchAllOrdersWithUser(ctx, db,
			queries.WithWhere("p.amount > $1", 200.00),
		)
		if err != nil {
			t.Fatalf("FetchAllOrdersWithUser failed: %v", err)
		}
		if len(ordersWithUser) != 1 {
			t.Fatalf("got %d orders, want 1", len(ordersWithUser))
		}
		if ordersWithUser[0].User == nil || ordersWithUser[0].User.ID != user.ID {
			t.Errorf("BelongsTo user not populated correctly")
		}
	})
}

func TestUpdateAndDelete(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	user := &models.User{Name: "Original Name", Email: "orig@test.com"}
	_ = queries.InsertUser(ctx, db, user)

	// 1. Update
	user.Name = "Updated Name"
	if err := queries.UpdateUser(ctx, db, user); err != nil {
		t.Fatalf("UpdateUser failed: %v", err)
	}

	fetched, _ := queries.GetUserByID(ctx, db, user.ID)
	if fetched.Name != "Updated Name" {
		t.Errorf("got name %q, want 'Updated Name'", fetched.Name)
	}

	// 2. Delete
	if err := queries.DeleteUser(ctx, db, user.ID); err != nil {
		t.Fatalf("DeleteUser failed: %v", err)
	}

	exists, _ := queries.ExistsUserByID(ctx, db, user.ID)
	if exists {
		t.Errorf("expected user to be deleted, but still exists")
	}
}

func TestTransactionSupport(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}

	user := &models.User{Name: "Tx User", Email: "tx@test.com"}

	if err := queries.InsertUser(ctx, tx, user); err != nil {
		_ = tx.Rollback()
		t.Fatalf("InsertUser in transaction failed: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}

	exists, err := queries.ExistsUserByID(ctx, db, user.ID)
	if err != nil {
		t.Fatalf("ExistsUserByID failed: %v", err)
	}
	if exists {
		t.Errorf("user should not exist after transaction rollback")
	}
}
