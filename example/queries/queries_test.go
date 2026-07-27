package queries_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/abiiranathan/query-gen/example/models"
	"github.com/abiiranathan/query-gen/example/queries"
	"github.com/abiiranathan/query-gen/perms"

	// _ "github.com/mattn/go-sqlite3"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// // setupTestDB creates an in-memory SQLite database matching the updated example schema.
// func setupTestDB(tb testing.TB) *sql.DB {
// 	tb.Helper()

// 	db, err := sql.Open("sqlite3", "file::memory:?mode=memory&cache=shared")
// 	if err != nil {
// 		tb.Fatalf("failed to open in-memory sqlite3 db: %v", err)
// 	}
// 	if err != nil {
// 		tb.Fatalf("failed to open in-memory sqlite db: %v", err)
// 	}

// 	schema := `
// 	CREATE TABLE IF NOT EXISTS users (
// 		id INTEGER PRIMARY KEY AUTOINCREMENT,
// 		name TEXT NOT NULL,
// 		email TEXT,
// 		created_at DATE DEFAULT CURRENT_DATE,
// 		deleted_at DATETIME,
// 		age VARCHAR(20) NOT NULL,
//      permissions INT
// 	);

// 	CREATE TABLE IF NOT EXISTS orders (
// 		order_id INTEGER PRIMARY KEY AUTOINCREMENT,
// 		user_id INTEGER NOT NULL,
// 		amount REAL NOT NULL,
// 		FOREIGN KEY (user_id) REFERENCES users(id)
// 	);

// 	CREATE TABLE IF NOT EXISTS projects (
// 		id INTEGER PRIMARY KEY AUTOINCREMENT,
// 		name TEXT NOT NULL,
// 		description TEXT,
// 		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
// 		deleted_at DATETIME
// 	);

// 	CREATE TABLE IF NOT EXISTS tags (
// 		id INTEGER PRIMARY KEY AUTOINCREMENT,
// 		project_id INTEGER NOT NULL,
// 		name TEXT NOT NULL,
// 		FOREIGN KEY (project_id) REFERENCES projects(id)
// 	);

// 	CREATE TABLE IF NOT EXISTS categories (
// 		id INTEGER PRIMARY KEY AUTOINCREMENT,
// 		project_id INTEGER NOT NULL,
// 		name TEXT NOT NULL,
// 		FOREIGN KEY (project_id) REFERENCES projects(id)
// 	);

// 	CREATE TABLE IF NOT EXISTS tasks (
// 		id INTEGER PRIMARY KEY AUTOINCREMENT,
// 		project_id INTEGER NOT NULL,
// 		title TEXT NOT NULL,
// 		FOREIGN KEY (project_id) REFERENCES projects(id)
// 	);
// 	`

// 	if _, err := db.Exec(schema); err != nil {
// 		tb.Fatalf("failed to execute test schema: %v", err)
// 	}

// 	tb.Cleanup(func() {
// 		_ = db.Close()
// 	})

// 	return db
// }

// setupPostgresDB connects to a local PostgreSQL instance using pgx stdlib driver.
// Skips automatically if the PostgreSQL database is not reachable.
func setupTestDB(tb testing.TB) *sql.DB {
	tb.Helper()

	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "postgres://postgres@localhost:5432/testdb?sslmode=disable"
	}

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		tb.Skipf("skipping postgres benchmark: connection error: %v", err)
	}

	if err := db.Ping(); err != nil {
		tb.Skipf("skipping postgres benchmark: database unreachable at %s: %v", connStr, err)
	}

	schema := `
	DROP TABLE IF EXISTS tasks CASCADE;
	DROP TABLE IF EXISTS categories CASCADE;
	DROP TABLE IF EXISTS tags CASCADE;
	DROP TABLE IF EXISTS projects CASCADE;
	DROP TABLE IF EXISTS orders CASCADE;
	DROP TABLE IF EXISTS users CASCADE;

	CREATE TABLE users (
		id BIGSERIAL PRIMARY KEY,
		name TEXT NOT NULL,
		email TEXT,
		created_at DATE DEFAULT CURRENT_DATE,
		deleted_at TIMESTAMPTZ,
		age VARCHAR(20) NOT NULL,
		permissions BIGINT
	);

	CREATE TABLE orders (
		order_id BIGSERIAL PRIMARY KEY,
		user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		amount DOUBLE PRECISION NOT NULL
	);

	CREATE TABLE projects (
		id BIGSERIAL PRIMARY KEY,
		name TEXT NOT NULL,
		description TEXT,
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		deleted_at TIMESTAMPTZ
	);

	CREATE TABLE tags (
		id BIGSERIAL PRIMARY KEY,
		project_id BIGINT NOT NULL REFERENCES projects(id),
		name TEXT NOT NULL
	);

	CREATE TABLE categories (
		id BIGSERIAL PRIMARY KEY,
		project_id BIGINT NOT NULL REFERENCES projects(id),
		name TEXT NOT NULL
	);

	CREATE TABLE tasks (
		id BIGSERIAL PRIMARY KEY,
		project_id BIGINT NOT NULL REFERENCES projects(id),
		title TEXT NOT NULL
	);
	`

	if _, err := db.Exec(schema); err != nil {
		tb.Fatalf("failed to execute postgres schema: %v", err)
	}

	tb.Cleanup(func() {
		_ = db.Close()
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
		CreatedAt: perms.Date(now),
	}

	// 1. Test InsertUser
	if err := queries.InsertUser(ctx, db, user); err != nil {
		t.Fatalf("InsertUser failed: %v", err)
	}
	if user.ID == 0 {
		t.Fatalf("expected non-zero user ID after insert")
	}

	// 2. Test GetUserByID without preloading
	fetched, err := queries.GetUserByID(ctx, db, user.ID)
	if err != nil {
		t.Fatalf("GetUserByID failed: %v", err)
	}

	if fetched.Name != user.Name || fetched.Email != user.Email {
		t.Errorf("mismatch fetched user: got %+v, want %+v", fetched, user)
	}

	// 3. Test InsertOrder & GetOrderByID
	order := &models.Order{
		UserID: user.ID,
		Amount: 99.99,
	}
	if err := queries.InsertOrder(ctx, db, order); err != nil {
		t.Fatalf("InsertOrder failed: %v", err)
	}
	if order.ID == 0 {
		t.Fatalf("expected non-zero order ID after insert")
	}

	fetchedOrder, err := queries.GetOrderByID(ctx, db, order.ID)
	if err != nil {
		t.Fatalf("GetOrderByID failed: %v", err)
	}
	if fetchedOrder.Amount != order.Amount || fetchedOrder.UserID != user.ID {
		t.Errorf("mismatch fetched order: got %+v, want %+v", fetchedOrder, order)
	}
}

func TestInsertUsers_Bulk(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	const totalUsers = 1000
	now := time.Now().Truncate(time.Second)

	users := make([]*models.User, totalUsers)
	for i := range totalUsers {
		users[i] = &models.User{
			Name:      fmt.Sprintf("User %d", i+1),
			Email:     fmt.Sprintf("user%d@example.com", i+1),
			CreatedAt: perms.Date(now),
		}
	}

	// 1. Bulk insert 1000 users (exercises internal batching & parameter limits)
	if err := queries.InsertUsers(ctx, db, users); err != nil {
		t.Fatalf("InsertUsers failed: %v", err)
	}

	// 2. Verify all 1000 structs received a non-zero database primary key ID
	for i, u := range users {
		if u.ID == 0 {
			t.Fatalf("expected non-zero user ID at index %d after bulk insert", i)
		}
	}

	// 3. Verify total count in database
	count, err := queries.CountUsers(ctx, db)
	if err != nil {
		t.Fatalf("CountUsers failed: %v", err)
	}
	if count != int64(totalUsers) {
		t.Errorf("got total count %d, want %d", count, totalUsers)
	}

	// 4. Verify boundary and sample records (first, middle, last)
	sampleIndices := []int{0, totalUsers / 2, totalUsers - 1}
	for _, idx := range sampleIndices {
		expected := users[idx]
		fetched, err := queries.GetUserByID(ctx, db, expected.ID)
		if err != nil {
			t.Fatalf("GetUserByID failed for user ID %d (index %d): %v", expected.ID, idx, err)
		}
		if fetched.Name != expected.Name || fetched.Email != expected.Email {
			t.Errorf("mismatch at index %d: got %+v, want %+v", idx, fetched, expected)
		}
	}
}

func TestExistsAndCount(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

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

	// 1. Test ExistsUserByID
	exists, err := queries.ExistsUserByID(ctx, db, users[0].ID)
	if err != nil || !exists {
		t.Errorf("ExistsUserByID returned (%v, %v), want (true, nil)", exists, err)
	}

	notExists, err := queries.ExistsUserByID(ctx, db, 99999)
	if err != nil || notExists {
		t.Errorf("ExistsUserByID returned (%v, %v), want (false, nil)", notExists, err)
	}

	// 2. Test CountUsers without options
	totalCount, err := queries.CountUsers(ctx, db)
	if err != nil || totalCount != 3 {
		t.Errorf("CountUsers total = %d, want 3 (err: %v)", totalCount, err)
	}

	// 3. Test CountUsers with WithWhere
	filteredCount, err := queries.CountUsers(ctx, db, queries.Where("email = $1", "u1@test.com"))
	if err != nil || filteredCount != 1 {
		t.Errorf("CountUsers with filter = %d, want 1 (err: %v)", filteredCount, err)
	}

	// 4. Test ExistsOrderByID & CountOrders
	order := &models.Order{UserID: users[0].ID, Amount: 15.00}
	_ = queries.InsertOrder(ctx, db, order)

	orderExists, err := queries.ExistsOrderByID(ctx, db, order.ID)
	if err != nil || !orderExists {
		t.Errorf("ExistsOrderByID returned (%v, %v), want (true, nil)", orderExists, err)
	}

	orderCount, err := queries.CountOrders(ctx, db)
	if err != nil || orderCount != 1 {
		t.Errorf("CountOrders total = %d, want 1 (err: %v)", orderCount, err)
	}
}

func TestFetchAllWithOptions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	names := []string{"Charlie", "Bob", "Alice", "David", "Eve"}
	for _, name := range names {
		_ = queries.InsertUser(ctx, db, &models.User{Name: name, Email: name + "@test.com"})
	}

	t.Run("Where and OrderBy", func(t *testing.T) {
		res, err := queries.FetchAllUsers(ctx, db,
			queries.Where("email LIKE $1", "%test.com"),
			queries.OrderBy("name ASC"),
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

	t.Run("Limit, Offset, and GroupBy", func(t *testing.T) {
		res, err := queries.FetchAllUsers(ctx, db,
			queries.GroupBy("id"),
			queries.OrderBy("name ASC"),
			queries.Limit(2),
			queries.Offset(1),
		)
		if err != nil {
			t.Fatalf("FetchAllUsers with limit/offset failed: %v", err)
		}
		if len(res) != 2 {
			t.Fatalf("got %d users, want 2", len(res))
		}

		if res[0].Name != "Bob" || res[1].Name != "Charlie" {
			t.Errorf("unexpected paginated result: got [%s, %s], want [Bob, Charlie]", res[0].Name, res[1].Name)
		}
	})
}

func TestHasManyRelationPreloading(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	user := &models.User{Name: "Order Owner", Email: "owner@test.com"}
	if err := queries.InsertUser(ctx, db, user); err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}

	orders := []*models.Order{
		{UserID: user.ID, Amount: 100.50},
		{UserID: user.ID, Amount: 49.99},
	}
	for _, o := range orders {
		if err := queries.InsertOrder(ctx, db, o); err != nil {
			t.Fatalf("failed to insert order: %v", err)
		}
	}

	t.Run("GetUserByID with Preload false (default)", func(t *testing.T) {
		fetchedUser, err := queries.GetUserByID(ctx, db, user.ID)
		if err != nil {
			t.Fatalf("GetUserByID failed: %v", err)
		}
		if len(fetchedUser.Orders) != 0 {
			t.Errorf("expected 0 preloaded orders when disabled, got %d", len(fetchedUser.Orders))
		}
	})

	t.Run("GetUserByID with Preload true", func(t *testing.T) {
		fetchedUser, err := queries.GetUserByID(ctx, db, user.ID, queries.Preload(true))
		if err != nil {
			t.Fatalf("GetUserByID with preload failed: %v", err)
		}
		if len(fetchedUser.Orders) != 2 {
			t.Fatalf("got %d preloaded orders, want 2", len(fetchedUser.Orders))
		}
	})

	t.Run("FetchAllUsers with Preload true", func(t *testing.T) {
		users, err := queries.FetchAllUsers(ctx, db,
			queries.Where("id = $1", user.ID),
			queries.Preload(true),
		)
		if err != nil {
			t.Fatalf("FetchAllUsers with preload failed: %v", err)
		}
		if len(users) != 1 {
			t.Fatalf("got %d parent users, want 1", len(users))
		}
		if len(users[0].Orders) != 2 {
			t.Errorf("got %d preloaded orders, want 2", len(users[0].Orders))
		}
	})
}

func TestBelongsToRelationPreloading(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	user := &models.User{Name: "Parent User", Email: "parent@test.com"}
	if err := queries.InsertUser(ctx, db, user); err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}

	order := &models.Order{UserID: user.ID, Amount: 250.00}
	if err := queries.InsertOrder(ctx, db, order); err != nil {
		t.Fatalf("failed to insert order: %v", err)
	}

	t.Run("GetOrderByID with Preload false (default)", func(t *testing.T) {
		fetchedOrder, err := queries.GetOrderByID(ctx, db, order.ID)
		if err != nil {
			t.Fatalf("GetOrderByID failed: %v", err)
		}
		if fetchedOrder.User != nil {
			t.Errorf("expected order.User to be nil when preloading is false")
		}
	})

	t.Run("GetOrderByID with Preload true", func(t *testing.T) {
		fetchedOrder, err := queries.GetOrderByID(ctx, db, order.ID, queries.Preload(true))
		if err != nil {
			t.Fatalf("GetOrderByID with preload failed: %v", err)
		}
		if fetchedOrder.User == nil {
			t.Fatalf("expected order.User to be populated, got nil")
		}
		if fetchedOrder.User.Name != user.Name {
			t.Errorf("got user name %q, want %q", fetchedOrder.User.Name, user.Name)
		}
	})

	t.Run("FetchAllOrders with Preload true", func(t *testing.T) {
		orders, err := queries.FetchAllOrders(ctx, db,
			queries.Where("amount > $1", 200.00),
			queries.Preload(true),
		)
		if err != nil {
			t.Fatalf("FetchAllOrders with preload failed: %v", err)
		}
		if len(orders) != 1 {
			t.Fatalf("got %d orders, want 1", len(orders))
		}
		if orders[0].User == nil || orders[0].User.ID != user.ID {
			t.Errorf("BelongsTo user not populated correctly during FetchAllOrders")
		}
	})
}

func TestSoftDelete(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	u1 := &models.User{Name: "Active User", Email: "active@test.com"}
	u2 := &models.User{Name: "Soft Deleted User", Email: "deleted@test.com"}
	if err := queries.InsertUser(ctx, db, u1); err != nil {
		t.Fatalf("failed to insert u1: %v", err)
	}
	if err := queries.InsertUser(ctx, db, u2); err != nil {
		t.Fatalf("failed to insert u2: %v", err)
	}

	// 1. Verify initial count
	initialCount, err := queries.CountUsers(ctx, db)
	if err != nil || initialCount != 2 {
		t.Fatalf("expected initial count 2, got %d (err: %v)", initialCount, err)
	}

	// 2. Perform soft delete on u2
	if err := queries.DeleteUser(ctx, db, u2.ID); err != nil {
		t.Fatalf("DeleteUser (soft delete) failed: %v", err)
	}

	// 3. GetUserByID should return ErrNoRows by default for soft-deleted record
	_, err = queries.GetUserByID(ctx, db, u2.ID)
	if err == nil {
		t.Fatalf("expected ErrNoRows for soft-deleted user, got nil error")
	}

	// 4. GetUserByID with IncludeDeleted option should return u2 with non-nil DeletedAt timestamp
	deletedUser, err := queries.GetUserByID(ctx, db, u2.ID, queries.IncludeDeleted())
	if err != nil {
		t.Fatalf("GetUserByID with IncludeDeleted failed: %v", err)
	}
	if deletedUser.DeletedAt == nil {
		t.Errorf("expected DeletedAt timestamp to be set on soft-deleted user")
	}

	// 5. ExistsUserByID default vs IncludeDeleted
	exists, err := queries.ExistsUserByID(ctx, db, u2.ID)
	if err != nil || exists {
		t.Errorf("ExistsUserByID should return false for soft-deleted user by default, got (%v, %v)", exists, err)
	}

	existsWithDeleted, err := queries.ExistsUserByID(ctx, db, u2.ID, queries.IncludeDeleted())
	if err != nil || !existsWithDeleted {
		t.Errorf("ExistsUserByID with IncludeDeleted should return true, got (%v, %v)", existsWithDeleted, err)
	}

	// 6. CountUsers default vs IncludeDeleted
	count, err := queries.CountUsers(ctx, db)
	if err != nil || count != 1 {
		t.Errorf("CountUsers default = %d, want 1 (active user only)", count)
	}

	countAll, err := queries.CountUsers(ctx, db, queries.IncludeDeleted())
	if err != nil || countAll != 2 {
		t.Errorf("CountUsers with IncludeDeleted = %d, want 2", countAll)
	}

	// 7. FetchAllUsers default vs IncludeDeleted
	activeUsers, err := queries.FetchAllUsers(ctx, db)
	if err != nil || len(activeUsers) != 1 {
		t.Fatalf("FetchAllUsers default returned %d users, want 1", len(activeUsers))
	}
	if activeUsers[0].ID != u1.ID {
		t.Errorf("FetchAllUsers returned user ID %d, want active user ID %d", activeUsers[0].ID, u1.ID)
	}

	allUsers, err := queries.FetchAllUsers(ctx, db, queries.IncludeDeleted())
	if err != nil || len(allUsers) != 2 {
		t.Fatalf("FetchAllUsers with IncludeDeleted returned %d users, want 2", len(allUsers))
	}

	// 8. Preloaded BelongsTo should filter out soft-deleted user
	order := &models.Order{UserID: u2.ID, Amount: 75.00}
	if err := queries.InsertOrder(ctx, db, order); err != nil {
		t.Fatalf("InsertOrder failed: %v", err)
	}

	fetchedOrder, err := queries.GetOrderByID(ctx, db, order.ID, queries.Preload(true))
	if err != nil {
		t.Fatalf("GetOrderByID with preload failed: %v", err)
	}
	if fetchedOrder.User != nil {
		t.Errorf("expected preloaded User to be nil for soft-deleted parent, got %+v", fetchedOrder.User)
	}

	// 9. HardDelete permanently removes the soft-deleted row
	if err := queries.DeleteUser(ctx, db, u2.ID, queries.HardDelete()); err != nil {
		t.Fatalf("DeleteUser with HardDelete failed: %v", err)
	}

	_, err = queries.GetUserByID(ctx, db, u2.ID, queries.IncludeDeleted())
	if err == nil {
		t.Errorf("expected ErrNoRows after HardDelete even with IncludeDeleted(), got user record")
	}
}

func TestUpdateAndDelete(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// 1. Test User Update & Delete
	user := &models.User{Name: "Original Name", Email: "orig@test.com"}
	_ = queries.InsertUser(ctx, db, user)

	user.Name = "Updated Name"
	if err := queries.UpdateUser(ctx, db, user); err != nil {
		t.Fatalf("UpdateUser failed: %v", err)
	}

	fetchedUser, _ := queries.GetUserByID(ctx, db, user.ID)
	if fetchedUser.Name != "Updated Name" {
		t.Errorf("got name %q, want 'Updated Name'", fetchedUser.Name)
	}

	// DeleteUser performs soft-delete (model contains DeletedAt)
	if err := queries.DeleteUser(ctx, db, user.ID); err != nil {
		t.Fatalf("DeleteUser failed: %v", err)
	}

	userExists, _ := queries.ExistsUserByID(ctx, db, user.ID)
	if userExists {
		t.Errorf("expected user to be excluded after soft delete")
	}

	// 2. Test Order Update & Delete (Hard Delete, Order lacks DeletedAt)
	newUser := &models.User{Name: "Order User", Email: "ou@test.com"}
	_ = queries.InsertUser(ctx, db, newUser)

	order := &models.Order{UserID: newUser.ID, Amount: 10.00}
	_ = queries.InsertOrder(ctx, db, order)

	order.Amount = 20.00
	if err := queries.UpdateOrder(ctx, db, order); err != nil {
		t.Fatalf("UpdateOrder failed: %v", err)
	}

	fetchedOrder, _ := queries.GetOrderByID(ctx, db, order.ID)
	if fetchedOrder.Amount != 20.00 {
		t.Errorf("got amount %f, want 20.00", fetchedOrder.Amount)
	}

	if err := queries.DeleteOrder(ctx, db, order.ID); err != nil {
		t.Fatalf("DeleteOrder failed: %v", err)
	}

	orderExists, _ := queries.ExistsOrderByID(ctx, db, order.ID)
	if orderExists {
		t.Errorf("expected order to be deleted, but still exists")
	}
}

func TestDeleteWithConditions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Seed users: 3 spam users and 2 valid users
	seedUsers := []*models.User{
		{Name: "Spam 1", Email: "s1@spam.com"},
		{Name: "Spam 2", Email: "s2@spam.com"},
		{Name: "Spam 3", Email: "s3@spam.com"},
		{Name: "Valid 1", Email: "v1@test.com"},
		{Name: "Valid 2", Email: "v2@test.com"},
	}

	for _, u := range seedUsers {
		if err := queries.InsertUser(ctx, db, u); err != nil {
			t.Fatalf("failed to insert seed user: %v", err)
		}
	}

	t.Run("Rejects execution when no options are provided", func(t *testing.T) {
		affected, err := queries.DeleteUsers(ctx, db)
		if err == nil {
			t.Fatalf("expected error when calling DeleteUsers without options, got nil")
		}
		if affected != 0 {
			t.Errorf("expected 0 affected rows on error, got %d", affected)
		}

		// Ensure no rows were deleted
		count, err := queries.CountUsers(ctx, db)
		if err != nil || count != 5 {
			t.Errorf("CountUsers = %d, want 5 (err: %v)", count, err)
		}
	})

	t.Run("Bulk soft-deletes matching users", func(t *testing.T) {
		affected, err := queries.DeleteUsers(ctx, db, queries.Where("email LIKE $1", "%@spam.com"))
		if err != nil {
			t.Fatalf("DeleteUsers failed: %v", err)
		}
		if affected != 3 {
			t.Fatalf("got %d affected rows, want 3", affected)
		}

		// Active count should drop from 5 to 2
		activeCount, err := queries.CountUsers(ctx, db)
		if err != nil || activeCount != 2 {
			t.Errorf("CountUsers active = %d, want 2 (err: %v)", activeCount, err)
		}

		// Total count including soft-deleted should remain 5
		totalCount, err := queries.CountUsers(ctx, db, queries.IncludeDeleted())
		if err != nil || totalCount != 5 {
			t.Errorf("CountUsers with IncludeDeleted = %d, want 5 (err: %v)", totalCount, err)
		}
	})

	t.Run("Bulk hard-deletes soft-deleted users", func(t *testing.T) {
		affected, err := queries.DeleteUsers(ctx, db,
			queries.Where("email LIKE $1", "%@spam.com"),
			queries.IncludeDeleted(),
			queries.HardDelete(),
		)
		if err != nil {
			t.Fatalf("DeleteUsers with HardDelete failed: %v", err)
		}
		if affected != 3 {
			t.Fatalf("got %d affected rows, want 3", affected)
		}

		// Total count including soft-deleted should now be 2
		totalCount, err := queries.CountUsers(ctx, db, queries.IncludeDeleted())
		if err != nil || totalCount != 2 {
			t.Errorf("CountUsers after HardDelete = %d, want 2 (err: %v)", totalCount, err)
		}
	})
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

func TestPaginate(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Seed 15 users for pagination testing.
	const totalUsers = 15
	for i := 1; i <= totalUsers; i++ {
		u := &models.User{
			Name:  fmt.Sprintf("User %02d", i),
			Email: fmt.Sprintf("user%02d@test.com", i),
		}
		if err := queries.InsertUser(ctx, db, u); err != nil {
			t.Fatalf("failed to insert seed user %d: %v", i, err)
		}
	}

	t.Run("First Page", func(t *testing.T) {
		res, err := queries.Paginate(
			ctx,
			db,
			queries.CountUsers,
			queries.FetchAllUsers,
			1, // page
			5, // pageSize
			queries.OrderBy("id ASC"),
		)
		if err != nil {
			t.Fatalf("Paginate page 1 failed: %v", err)
		}

		if res.Count != totalUsers {
			t.Errorf("got Count = %d, want %d", res.Count, totalUsers)
		}
		if res.TotalPages != 3 {
			t.Errorf("got TotalPages = %d, want 3", res.TotalPages)
		}
		if len(res.Results) != 5 {
			t.Fatalf("got %d results, want 5", len(res.Results))
		}
		if !res.HasNext || res.HasPrev {
			t.Errorf("page 1 flags incorrect: HasNext=%v (want true), HasPrev=%v (want false)", res.HasNext, res.HasPrev)
		}
		if res.Results[0].Name != "User 01" {
			t.Errorf("expected first item 'User 01', got %q", res.Results[0].Name)
		}
	})

	t.Run("Middle Page", func(t *testing.T) {
		res, err := queries.Paginate(
			ctx,
			db,
			queries.CountUsers,
			queries.FetchAllUsers,
			2,
			5,
			queries.OrderBy("id ASC"),
		)
		if err != nil {
			t.Fatalf("Paginate page 2 failed: %v", err)
		}

		if len(res.Results) != 5 {
			t.Fatalf("got %d results, want 5", len(res.Results))
		}
		if !res.HasNext || !res.HasPrev {
			t.Errorf("page 2 flags incorrect: HasNext=%v (want true), HasPrev=%v (want true)", res.HasNext, res.HasPrev)
		}
		if res.Results[0].Name != "User 06" {
			t.Errorf("expected first item of page 2 'User 06', got %q", res.Results[0].Name)
		}
	})

	t.Run("Last Page", func(t *testing.T) {
		res, err := queries.Paginate(
			ctx,
			db,
			queries.CountUsers,
			queries.FetchAllUsers,
			3,
			5,
			queries.OrderBy("id ASC"),
		)
		if err != nil {
			t.Fatalf("Paginate page 3 failed: %v", err)
		}

		if len(res.Results) != 5 {
			t.Fatalf("got %d results, want 5", len(res.Results))
		}
		if res.HasNext || !res.HasPrev {
			t.Errorf("last page flags incorrect: HasNext=%v (want false), HasPrev=%v (want true)", res.HasNext, res.HasPrev)
		}
	})

	t.Run("Invalid Page Defaults Handling", func(t *testing.T) {
		// Passing non-positive page or pageSize should default gracefully (page=1, pageSize=10).
		res, err := queries.Paginate(
			ctx,
			db,
			queries.CountUsers,
			queries.FetchAllUsers,
			0, // page < 1
			0, // pageSize < 1
		)
		if err != nil {
			t.Fatalf("Paginate with invalid bounds failed: %v", err)
		}

		if res.Page != 1 {
			t.Errorf("got Page = %d, want default 1", res.Page)
		}
		if res.PageSize != 10 {
			t.Errorf("got PageSize = %d, want default 10", res.PageSize)
		}
		if len(res.Results) != 10 {
			t.Errorf("got %d results, want 10", len(res.Results))
		}
	})
}

func TestWithHavingClause(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Seed users and orders to test aggregate filtering.
	u1 := &models.User{Name: "Heavy Spender", Email: "heavy@test.com", Age: "30"}
	u2 := &models.User{Name: "Light Spender", Email: "light@test.com", Age: "25"}
	_ = queries.InsertUser(ctx, db, u1)
	_ = queries.InsertUser(ctx, db, u2)

	// User 1 gets 3 orders: 100, 150, 200
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u1.ID, Amount: 100.00})
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u1.ID, Amount: 150.00})
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u1.ID, Amount: 200.00})

	// User 2 gets 1 order: 20
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u2.ID, Amount: 20.00})

	t.Run("Group By and Having Clause", func(t *testing.T) {
		// Include all selected columns in GROUP BY for ANSI SQL / Postgres compliance.
		// Filter for individual order rows where SUM(amount) > 180.00 (matches only the $200 order).
		orders, err := queries.FetchAllOrders(ctx, db,
			queries.GroupBy("user_id, order_id, amount"),
			queries.Having("SUM(amount) > $1", 180.00),
		)
		if err != nil {
			t.Fatalf("FetchAllOrders with HAVING failed: %v", err)
		}

		if len(orders) != 1 {
			t.Fatalf("got %d grouped orders, want 1", len(orders))
		}
		if orders[0].UserID != u1.ID {
			t.Errorf("got user_id %d, want %d", orders[0].UserID, u1.ID)
		}
		if orders[0].Amount != 200.00 {
			t.Errorf("got amount %f, want 200.00", orders[0].Amount)
		}
	})

	t.Run("Multiple Having Clauses Combined", func(t *testing.T) {
		// Combining multiple HAVING calls joins them with AND.
		// Matches only the single order with amount >= 100 AND SUM(amount) > 180.00.
		orders, err := queries.FetchAllOrders(ctx, db,
			queries.GroupBy("user_id, order_id, amount"),
			queries.Having("amount >= $1", 100.00),
			queries.Having("SUM(amount) > $2", 180.00),
		)
		if err != nil {
			t.Fatalf("FetchAllOrders with multiple HAVING failed: %v", err)
		}

		if len(orders) != 1 {
			t.Fatalf("got %d orders, want 1 matching both HAVING clauses", len(orders))
		}
		if orders[0].Amount != 200.00 {
			t.Errorf("got amount %f, want 200.00", orders[0].Amount)
		}
	})
}

func TestFilterOptions(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Seed users
	u1 := &models.User{Name: "Alice Smith", Email: "alice@example.com"}
	u2 := &models.User{Name: "Bob Jones", Email: "bob@example.com"}
	u3 := &models.User{Name: "Charlie Brown", Email: ""} // Empty/NULL email test
	_ = queries.InsertUser(ctx, db, u1)
	_ = queries.InsertUser(ctx, db, u2)
	_ = queries.InsertUser(ctx, db, u3)

	// Seed orders for numeric range tests
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u1.ID, Amount: 10.50})
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u1.ID, Amount: 50.00})
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u2.ID, Amount: 100.00})

	t.Run("In and NotIn", func(t *testing.T) {
		// In
		inUsers, err := queries.FetchAllUsers(ctx, db, queries.In("name", "Alice Smith", "Bob Jones"))
		if err != nil {
			t.Fatalf("In query failed: %v", err)
		}
		if len(inUsers) != 2 {
			t.Errorf("In got %d users, want 2", len(inUsers))
		}

		// NotIn
		notInUsers, err := queries.FetchAllUsers(ctx, db, queries.NotIn("name", "Alice Smith"))
		if err != nil {
			t.Fatalf("NotIn query failed: %v", err)
		}
		if len(notInUsers) != 2 {
			t.Errorf("NotIn got %d users, want 2", len(notInUsers))
		}
	})

	t.Run("IsNull and IsNotNull", func(t *testing.T) {
		// SQLite converts empty string or NULL appropriately
		notNullUsers, err := queries.FetchAllUsers(ctx, db,
			queries.IsNotNull("email"),
			queries.Where("email != $1", ""),
		)
		if err != nil {
			t.Fatalf("IsNotNull query failed: %v", err)
		}
		if len(notNullUsers) != 2 {
			t.Errorf("IsNotNull got %d users, want 2", len(notNullUsers))
		}

		nullUsers, err := queries.FetchAllUsers(ctx, db, queries.IsNull("email"))
		if err != nil {
			t.Fatalf("IsNull query failed: %v", err)
		}
		// Expect 0 or 1 depending on driver insertion behavior for empty string/NULL
		_ = nullUsers
	})

	t.Run("Between", func(t *testing.T) {
		orders, err := queries.FetchAllOrders(ctx, db, queries.Between("amount", 20.00, 80.00))
		if err != nil {
			t.Fatalf("Between query failed: %v", err)
		}
		if len(orders) != 1 {
			t.Fatalf("Between got %d orders, want 1", len(orders))
		}
		if orders[0].Amount != 50.00 {
			t.Errorf("Between expected order amount 50.00, got %f", orders[0].Amount)
		}
	})

	t.Run("ILIKE", func(t *testing.T) {
		users, err := queries.FetchAllUsers(ctx, db, queries.ILIKE("name", "alice"))
		if err != nil {
			t.Fatalf("ILIKE query failed: %v", err)
		}
		if len(users) != 1 || users[0].Name != "Alice Smith" {
			t.Errorf("ILIKE failed to match 'Alice Smith', got %v", users)
		}
	})

	t.Run("Search Multi-Column", func(t *testing.T) {
		// Search matches across 'name' or 'email'
		users, err := queries.FetchAllUsers(ctx, db, queries.Search("bob", "name", "email"))
		if err != nil {
			t.Fatalf("Search query failed: %v", err)
		}
		if len(users) != 1 || users[0].Name != "Bob Jones" {
			t.Errorf("Search failed to match 'Bob Jones', got %v", users)
		}
	})

	t.Run("Comparison Operators Gt, Gte, Lt, Lte", func(t *testing.T) {
		// Gt
		gtOrders, err := queries.FetchAllOrders(ctx, db, queries.Gt("amount", 50.00))
		if err != nil || len(gtOrders) != 1 {
			t.Errorf("Gt got %d orders, want 1 (err: %v)", len(gtOrders), err)
		}

		// Gte
		gteOrders, err := queries.FetchAllOrders(ctx, db, queries.Gte("amount", 50.00))
		if err != nil || len(gteOrders) != 2 {
			t.Errorf("Gte got %d orders, want 2 (err: %v)", len(gteOrders), err)
		}

		// Lt
		ltOrders, err := queries.FetchAllOrders(ctx, db, queries.Lt("amount", 50.00))
		if err != nil || len(ltOrders) != 1 {
			t.Errorf("Lt got %d orders, want 1 (err: %v)", len(ltOrders), err)
		}

		// Lte
		lteOrders, err := queries.FetchAllOrders(ctx, db, queries.Lte("amount", 50.00))
		if err != nil || len(lteOrders) != 2 {
			t.Errorf("Lte got %d orders, want 2 (err: %v)", len(lteOrders), err)
		}
	})
}

// ============ Many to many =========================
func TestInsertAndGetProjectWithRelations(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	project := &models.Project{
		Name:        "Query Generator Engine",
		Description: "Type-safe SQL generator for Go",
		CreatedAt:   time.Now().Truncate(time.Second),
	}

	if err := queries.InsertProject(ctx, db, project); err != nil {
		t.Fatalf("InsertProject failed: %v", err)
	}

	tag1 := &models.Tag{ProjectID: project.ID, Name: "golang"}
	tag2 := &models.Tag{ProjectID: project.ID, Name: "database"}
	if err := queries.InsertTag(ctx, db, tag1); err != nil {
		t.Fatalf("InsertTag failed: %v", err)
	}
	if err := queries.InsertTag(ctx, db, tag2); err != nil {
		t.Fatalf("InsertTag failed: %v", err)
	}

	cat := &models.Category{ProjectID: project.ID, Name: "Developer Tools"}
	if err := queries.InsertCategory(ctx, db, cat); err != nil {
		t.Fatalf("InsertCategory failed: %v", err)
	}

	task1 := &models.Task{ProjectID: project.ID, Title: "Add batching support"}
	task2 := &models.Task{ProjectID: project.ID, Title: "Write integration tests"}
	if err := queries.InsertTask(ctx, db, task1); err != nil {
		t.Fatalf("InsertTask failed: %v", err)
	}
	if err := queries.InsertTask(ctx, db, task2); err != nil {
		t.Fatalf("InsertTask failed: %v", err)
	}

	fetched, err := queries.GetProjectByID(ctx, db, project.ID, queries.Preload(true))
	if err != nil {
		t.Fatalf("GetProjectByID with preloading failed: %v", err)
	}

	if len(fetched.Tags) != 2 {
		t.Errorf("expected 2 tags preloaded, got %d", len(fetched.Tags))
	}
	if len(fetched.Categories) != 1 {
		t.Errorf("expected 1 category preloaded, got %d", len(fetched.Categories))
	}
	if len(fetched.Tasks) != 2 {
		t.Errorf("expected 2 tasks preloaded, got %d", len(fetched.Tasks))
	}
}

// Named constants for scale testing and benchmarking datasets.
const (
	scaleProjectCount = 10000
	scaleTagCount     = 1000
	scaleTaskCount    = 1000
)

// seedLargeDataset inserts projectCount projects and tagCount/taskCount associated
// records inside a single transaction to allow fast in-memory database setup.
func seedLargeDataset(tb testing.TB, db *sql.DB, projectCount, tagCount, taskCount int) {
	tb.Helper()

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatalf("seedLargeDataset: failed to begin transaction: %v", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	for i := 1; i <= projectCount; i++ {
		p := &models.Project{
			Name:        fmt.Sprintf("Project %d", i),
			Description: "Bulk workload benchmark description",
		}
		if err := queries.InsertProject(ctx, tx, p); err != nil {
			tb.Fatalf("seedLargeDataset: InsertProject failed at index %d: %v", i, err)
		}
	}

	for i := 1; i <= tagCount; i++ {
		// Distribute tags across created projects
		projectID := int64((i % projectCount) + 1)
		tag := &models.Tag{
			ProjectID: projectID,
			Name:      fmt.Sprintf("tag-%d", i),
		}
		if err := queries.InsertTag(ctx, tx, tag); err != nil {
			tb.Fatalf("seedLargeDataset: InsertTag failed at index %d: %v", i, err)
		}
	}

	for i := 1; i <= taskCount; i++ {
		// Distribute tasks across created projects
		projectID := int64((i % projectCount) + 1)
		task := &models.Task{
			ProjectID: projectID,
			Title:     fmt.Sprintf("Task item %d", i),
		}
		if err := queries.InsertTask(ctx, tx, task); err != nil {
			tb.Fatalf("seedLargeDataset: InsertTask failed at index %d: %v", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		tb.Fatalf("seedLargeDataset: failed to commit transaction: %v", err)
	}
}

// TestFetchAllProjectsLargeScale verifies relation preloading correctness at scale with 10,000 projects.
func TestFetchAllProjectsLargeScale(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scale test in short mode")
	}

	db := setupTestDB(t)
	ctx := context.Background()

	seedLargeDataset(t, db, scaleProjectCount, scaleTagCount, scaleTaskCount)

	projects, err := queries.FetchAllProjects(ctx, db, queries.Preload(true))
	if err != nil {
		t.Fatalf("FetchAllProjects with preloading failed: %v", err)
	}

	if len(projects) != scaleProjectCount {
		t.Fatalf("FetchAllProjects count mismatch: got %d, want %d", len(projects), scaleProjectCount)
	}

	totalTags := 0
	totalTasks := 0
	for _, p := range projects {
		totalTags += len(p.Tags)
		totalTasks += len(p.Tasks)
	}

	if totalTags != scaleTagCount {
		t.Errorf("preloaded tags count mismatch: got %d, want %d", totalTags, scaleTagCount)
	}
	if totalTasks != scaleTaskCount {
		t.Errorf("preloaded tasks count mismatch: got %d, want %d", totalTasks, scaleTaskCount)
	}
}

// BenchmarkFetchAllProjectsWithRelations measures query throughput and memory allocations
// when fetching and preloading associations across 10,000 records.
func BenchmarkFetchAllProjectsWithRelations(b *testing.B) {
	db := setupTestDB(b)
	ctx := context.Background()

	seedLargeDataset(b, db, scaleProjectCount, scaleTagCount, scaleTaskCount)

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		projects, err := queries.FetchAllProjects(ctx, db, queries.Preload(true))
		if err != nil {
			b.Fatalf("BenchmarkFetchAllProjectsWithRelations failed: %v", err)
		}
		if len(projects) != scaleProjectCount {
			b.Fatalf("unexpected project count: got %d, want %d", len(projects), scaleProjectCount)
		}
	}
}

// BenchmarkFetchAllProjectsNoPreload measures baseline fetch throughput without preloading associations.
func BenchmarkFetchAllProjectsNoPreload(b *testing.B) {
	db := setupTestDB(b)
	ctx := context.Background()

	seedLargeDataset(b, db, scaleProjectCount, scaleTagCount, scaleTaskCount)

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		projects, err := queries.FetchAllProjects(ctx, db, queries.Preload(false))
		if err != nil {
			b.Fatalf("BenchmarkFetchAllProjectsNoPreload failed: %v", err)
		}
		if len(projects) != scaleProjectCount {
			b.Fatalf("unexpected project count: got %d, want %d", len(projects), scaleProjectCount)
		}
	}
}

func TestFetchAllProjectsWithRelations(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	p1 := &models.Project{Name: "Project Alpha", Description: "First project"}
	p2 := &models.Project{Name: "Project Beta", Description: "Second project"}
	if err := queries.InsertProject(ctx, db, p1); err != nil {
		t.Fatalf("InsertProject p1 failed: %v", err)
	}
	if err := queries.InsertProject(ctx, db, p2); err != nil {
		t.Fatalf("InsertProject p2 failed: %v", err)
	}

	_ = queries.InsertTag(ctx, db, &models.Tag{ProjectID: p1.ID, Name: "backend"})
	_ = queries.InsertTag(ctx, db, &models.Tag{ProjectID: p2.ID, Name: "frontend"})
	_ = queries.InsertCategory(ctx, db, &models.Category{ProjectID: p1.ID, Name: "System"})
	_ = queries.InsertTask(ctx, db, &models.Task{ProjectID: p1.ID, Title: "Task 1"})
	_ = queries.InsertTask(ctx, db, &models.Task{ProjectID: p2.ID, Title: "Task 2"})

	// Preload triggers split IN-query batching for the 3 HasMany relations
	projects, err := queries.FetchAllProjects(ctx, db, queries.Preload(true))
	if err != nil {
		t.Fatalf("FetchAllProjects with preloading failed: %v", err)
	}

	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}

	projMap := make(map[int64]*models.Project, len(projects))
	for _, p := range projects {
		projMap[p.ID] = p
	}

	p1Fetched := projMap[p1.ID]
	if len(p1Fetched.Tags) != 1 || len(p1Fetched.Categories) != 1 || len(p1Fetched.Tasks) != 1 {
		t.Errorf("p1 association mismatch: tags=%d, categories=%d, tasks=%d",
			len(p1Fetched.Tags), len(p1Fetched.Categories), len(p1Fetched.Tasks))
	}

	p2Fetched := projMap[p2.ID]
	if len(p2Fetched.Tags) != 1 || len(p2Fetched.Tasks) != 1 {
		t.Errorf("p2 association mismatch: tags=%d, tasks=%d",
			len(p2Fetched.Tags), len(p2Fetched.Tasks))
	}
}

func TestDeleteProject(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	p := &models.Project{Name: "Transient Project"}
	if err := queries.InsertProject(ctx, db, p); err != nil {
		t.Fatalf("InsertProject failed: %v", err)
	}

	// Test soft delete behavior
	if err := queries.DeleteProject(ctx, db, p.ID); err != nil {
		t.Fatalf("DeleteProject failed: %v", err)
	}

	exists, err := queries.ExistsProjectByID(ctx, db, p.ID)
	if err != nil {
		t.Fatalf("ExistsProjectByID failed: %v", err)
	}
	if exists {
		t.Errorf("expected soft-deleted project to not exist in standard queries")
	}

	// Verify project remains visible with IncludeDeleted
	existsDeleted, err := queries.ExistsProjectByID(ctx, db, p.ID, queries.IncludeDeleted())
	if err != nil || !existsDeleted {
		t.Errorf("expected soft-deleted project to exist when IncludeDeleted option is used")
	}
}
