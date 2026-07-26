package queries_test

import (
	"context"
	"testing"
	"time"

	"github.com/abiiranathan/query-gen/example/models"
	"github.com/abiiranathan/query-gen/example/queries"
)

// setupCompositePKTable ensures the user_roles table exists in the test database.
func setupCompositePKTable(tb testing.TB, db queries.DBTX) {
	tb.Helper()

	schema := `
	DROP TABLE IF EXISTS user_roles;
	
	CREATE TABLE IF NOT EXISTS user_roles (
		user_id BIGINT NOT NULL,
		role_id BIGINT NOT NULL,
		assigned_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (user_id, role_id)
	);
	`
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		tb.Fatalf("failed to create user_roles table: %v", err)
	}
}

func TestCompositePK_InsertAndGetByID(t *testing.T) {
	db := setupTestDB(t)
	setupCompositePKTable(t, db)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	userRole := &models.UserRole{
		UserID:     101,
		RoleID:     201,
		AssignedAt: now,
	}

	// 1. Test InsertUserRole
	if err := queries.InsertUserRole(ctx, db, userRole); err != nil {
		t.Fatalf("InsertUserRole failed: %v", err)
	}

	// 2. Test GetUserRoleByID with multi-column composite key arguments (UserID, RoleID)
	fetched, err := queries.GetUserRoleByID(ctx, db, userRole.UserID, userRole.RoleID)
	if err != nil {
		t.Fatalf("GetUserRoleByID failed: %v", err)
	}

	if fetched.UserID != userRole.UserID || fetched.RoleID != userRole.RoleID {
		t.Errorf("mismatch fetched user role key: got (%d, %d), want (%d, %d)",
			fetched.UserID, fetched.RoleID, userRole.UserID, userRole.RoleID)
	}

	// 3. Querying non-existent composite PK record should return sql.ErrNoRows
	_, err = queries.GetUserRoleByID(ctx, db, 9999, 8888)
	if err == nil {
		t.Fatalf("expected error for non-existent composite PK, got nil")
	}
}

func TestCompositePK_ExistsAndCount(t *testing.T) {
	db := setupTestDB(t)
	setupCompositePKTable(t, db)
	ctx := context.Background()

	roles := []*models.UserRole{
		{UserID: 1, RoleID: 10},
		{UserID: 1, RoleID: 20},
		{UserID: 2, RoleID: 10},
	}

	for _, r := range roles {
		if err := queries.InsertUserRole(ctx, db, r); err != nil {
			t.Fatalf("failed to insert user role (%d, %d): %v", r.UserID, r.RoleID, err)
		}
	}

	// 1. Test ExistsUserRoleByID
	exists, err := queries.ExistsUserRoleByID(ctx, db, 1, 10)
	if err != nil || !exists {
		t.Errorf("ExistsUserRoleByID(1, 10) returned (%v, %v), want (true, nil)", exists, err)
	}

	notExists, err := queries.ExistsUserRoleByID(ctx, db, 1, 99)
	if err != nil || notExists {
		t.Errorf("ExistsUserRoleByID(1, 99) returned (%v, %v), want (false, nil)", notExists, err)
	}

	// 2. Test CountUserRoles without options
	totalCount, err := queries.CountUserRoles(ctx, db)
	if err != nil || totalCount != 3 {
		t.Errorf("CountUserRoles total = %d, want 3 (err: %v)", totalCount, err)
	}

	// 3. Test CountUserRoles with Where filter on composite column
	filteredCount, err := queries.CountUserRoles(ctx, db, queries.Where("user_id = $1", 1))
	if err != nil || filteredCount != 2 {
		t.Errorf("CountUserRoles with user_id filter = %d, want 2 (err: %v)", filteredCount, err)
	}
}

func TestCompositePK_UpdateAndDelete(t *testing.T) {
	db := setupTestDB(t)
	setupCompositePKTable(t, db)
	ctx := context.Background()

	userRole := &models.UserRole{
		UserID:     50,
		RoleID:     500,
		AssignedAt: time.Now().Add(-1 * time.Hour).Truncate(time.Second),
	}
	if err := queries.InsertUserRole(ctx, db, userRole); err != nil {
		t.Fatalf("InsertUserRole failed: %v", err)
	}

	// 1. Test UpdateUserRole
	updatedTime := time.Now().Truncate(time.Second)
	userRole.AssignedAt = updatedTime

	if err := queries.UpdateUserRole(ctx, db, userRole); err != nil {
		t.Fatalf("UpdateUserRole failed: %v", err)
	}

	fetched, err := queries.GetUserRoleByID(ctx, db, userRole.UserID, userRole.RoleID)
	if err != nil {
		t.Fatalf("GetUserRoleByID after update failed: %v", err)
	}
	if !fetched.AssignedAt.Equal(updatedTime) {
		t.Errorf("got AssignedAt %v, want %v", fetched.AssignedAt, updatedTime)
	}

	// 2. Test DeleteUserRole passing composite primary key components
	if err := queries.DeleteUserRole(ctx, db, userRole.UserID, userRole.RoleID); err != nil {
		t.Fatalf("DeleteUserRole failed: %v", err)
	}

	exists, err := queries.ExistsUserRoleByID(ctx, db, userRole.UserID, userRole.RoleID)
	if err != nil {
		t.Fatalf("ExistsUserRoleByID after delete check failed: %v", err)
	}
	if exists {
		t.Errorf("expected user role (%d, %d) to be deleted, but still exists", userRole.UserID, userRole.RoleID)
	}

	// 3. Deleting non-existent record should return sql.ErrNoRows error
	err = queries.DeleteUserRole(ctx, db, 9999, 9999)
	if err == nil {
		t.Errorf("expected error deleting non-existent composite PK, got nil")
	}
}

func TestCompositePK_FetchAllAndPaginate(t *testing.T) {
	db := setupTestDB(t)
	setupCompositePKTable(t, db)
	ctx := context.Background()

	// Seed composite PK records
	for u := int64(1); u <= 3; u++ {
		for r := int64(1); r <= 4; r++ {
			ur := &models.UserRole{UserID: u, RoleID: r}
			if err := queries.InsertUserRole(ctx, db, ur); err != nil {
				t.Fatalf("failed to insert seed user role (%d, %d): %v", u, r, err)
			}
		}
	}

	t.Run("FetchAllUserRoles with Ordering across Composite Columns", func(t *testing.T) {
		res, err := queries.FetchAllUserRoles(ctx, db,
			queries.Where("user_id = $1", 2),
			queries.OrderBy("role_id DESC"),
		)
		if err != nil {
			t.Fatalf("FetchAllUserRoles failed: %v", err)
		}

		if len(res) != 4 {
			t.Fatalf("got %d records, want 4", len(res))
		}
		if res[0].RoleID != 4 || res[3].RoleID != 1 {
			t.Errorf("ordering incorrect: first RoleID=%d (want 4), last RoleID=%d (want 1)",
				res[0].RoleID, res[3].RoleID)
		}
	})

	t.Run("Paginate Composite PK Model", func(t *testing.T) {
		res, err := queries.Paginate(
			ctx,
			db,
			queries.CountUserRoles,
			queries.FetchAllUserRoles,
			1, // page
			5, // pageSize
			queries.OrderBy("user_id ASC, role_id ASC"),
		)
		if err != nil {
			t.Fatalf("Paginate failed for composite PK model: %v", err)
		}

		if res.Count != 12 {
			t.Errorf("got Count = %d, want 12", res.Count)
		}
		if res.TotalPages != 3 {
			t.Errorf("got TotalPages = %d, want 3", res.TotalPages)
		}
		if len(res.Results) != 5 {
			t.Fatalf("got %d results on page 1, want 5", len(res.Results))
		}
		if res.Results[0].UserID != 1 || res.Results[0].RoleID != 1 {
			t.Errorf("unexpected first item: got (%d, %d), want (1, 1)",
				res.Results[0].UserID, res.Results[0].RoleID)
		}
	})
}

func TestCompositePK_BulkDelete(t *testing.T) {
	db := setupTestDB(t)
	setupCompositePKTable(t, db)
	ctx := context.Background()

	// Seed composite PK user roles: 5 for User 1, 3 for User 2
	for r := int64(1); r <= 5; r++ {
		_ = queries.InsertUserRole(ctx, db, &models.UserRole{UserID: 1, RoleID: r})
	}
	for r := int64(1); r <= 3; r++ {
		_ = queries.InsertUserRole(ctx, db, &models.UserRole{UserID: 2, RoleID: r})
	}

	t.Run("Bulk Delete with Where Clause", func(t *testing.T) {
		affected, err := queries.DeleteUserRoles(ctx, db, queries.Where("user_id = $1", 1))
		if err != nil {
			t.Fatalf("DeleteUserRoles bulk delete failed: %v", err)
		}
		if affected != 5 {
			t.Fatalf("got %d affected rows, want 5", affected)
		}

		remaining, err := queries.CountUserRoles(ctx, db)
		if err != nil || remaining != 3 {
			t.Errorf("remaining count = %d, want 3 (err: %v)", remaining, err)
		}
	})

	t.Run("Rejects Bulk Delete without Where Clause", func(t *testing.T) {
		_, err := queries.DeleteUserRoles(ctx, db)
		if err == nil {
			t.Fatalf("expected error when calling DeleteUserRoles without options, got nil")
		}
	})
}
