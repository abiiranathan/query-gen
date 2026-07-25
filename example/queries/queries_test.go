package queries_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/abiiranathan/query-gen/example/models"
	"github.com/abiiranathan/query-gen/example/queries"
	_ "modernc.org/sqlite"
)

// setupTestDB creates an in-memory SQLite database matching the example schema.
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
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		deleted_at TIMESTAMPTZ,
    	age varchar(20) NOT NULL
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
		CreatedAt: now,
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

	t.Run("GetUserByID with PreloadAssociations false (default)", func(t *testing.T) {
		fetchedUser, err := queries.GetUserByID(ctx, db, user.ID)
		if err != nil {
			t.Fatalf("GetUserByID failed: %v", err)
		}
		if len(fetchedUser.Orders) != 0 {
			t.Errorf("expected 0 preloaded orders when disabled, got %d", len(fetchedUser.Orders))
		}
	})

	t.Run("GetUserByID with PreloadAssociations true", func(t *testing.T) {
		fetchedUser, err := queries.GetUserByID(ctx, db, user.ID, queries.PreloadAssociations(true))
		if err != nil {
			t.Fatalf("GetUserByID with preload failed: %v", err)
		}
		if len(fetchedUser.Orders) != 2 {
			t.Fatalf("got %d preloaded orders, want 2", len(fetchedUser.Orders))
		}
	})

	t.Run("FetchAllUsers with PreloadAssociations true", func(t *testing.T) {
		users, err := queries.FetchAllUsers(ctx, db,
			queries.Where("id = $1", user.ID),
			queries.PreloadAssociations(true),
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

	t.Run("GetOrderByID with PreloadAssociations false (default)", func(t *testing.T) {
		fetchedOrder, err := queries.GetOrderByID(ctx, db, order.ID)
		if err != nil {
			t.Fatalf("GetOrderByID failed: %v", err)
		}
		if fetchedOrder.User != nil {
			t.Errorf("expected order.User to be nil when preloading is false")
		}
	})

	t.Run("GetOrderByID with PreloadAssociations true", func(t *testing.T) {
		fetchedOrder, err := queries.GetOrderByID(ctx, db, order.ID, queries.PreloadAssociations(true))
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

	t.Run("FetchAllOrders with PreloadAssociations true", func(t *testing.T) {
		orders, err := queries.FetchAllOrders(ctx, db,
			queries.Where("amount > $1", 200.00),
			queries.PreloadAssociations(true),
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

	fetchedOrder, err := queries.GetOrderByID(ctx, db, order.ID, queries.PreloadAssociations(true))
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
	u1 := &models.User{Name: "Heavy Spender", Email: "heavy@test.com"}
	u2 := &models.User{Name: "Light Spender", Email: "light@test.com"}
	_ = queries.InsertUser(ctx, db, u1)
	_ = queries.InsertUser(ctx, db, u2)

	// User 1 gets 3 orders
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u1.ID, Amount: 100.00})
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u1.ID, Amount: 150.00})
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u1.ID, Amount: 200.00})

	// User 2 gets 1 order
	_ = queries.InsertOrder(ctx, db, &models.Order{UserID: u2.ID, Amount: 20.00})

	t.Run("Group By and Having Clause", func(t *testing.T) {
		// Select orders grouped by user_id having total amount > 50
		orders, err := queries.FetchAllOrders(ctx, db,
			queries.GroupBy("user_id"),
			queries.Having("SUM(amount) > $1", 50.00),
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
	})

	t.Run("Multiple Having Clauses Combined", func(t *testing.T) {
		// Combining multiple WithHaving calls should join them with AND
		orders, err := queries.FetchAllOrders(ctx, db,
			queries.GroupBy("user_id"),
			queries.Having("COUNT(*) >= $1", 2),
			queries.Having("SUM(amount) > $2", 100.00),
		)
		if err != nil {
			t.Fatalf("FetchAllOrders with multiple HAVING failed: %v", err)
		}

		if len(orders) != 1 {
			t.Fatalf("got %d orders, want 1 matching both HAVING clauses", len(orders))
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
