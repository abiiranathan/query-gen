package queries_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite" // Pure Go SQLite driver for testing

	"github.com/abiiranathan/query-gen/example/models"
	"github.com/abiiranathan/query-gen/example/queries"
)

// setupSQLite3TestDB initializes an in-memory SQLite database and creates target tables.
func setupSQLite3TestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("Failed to open SQLite in-memory database: %v", err)
	}

	schemaDDL := `
	CREATE TABLE courses (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE,
		title TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		deleted_at DATETIME
	);

	CREATE TABLE students (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		email TEXT NOT NULL UNIQUE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		deleted_at DATETIME
	);

	CREATE TABLE student_courses (
		student_id INTEGER NOT NULL,
		course_id INTEGER NOT NULL,
		PRIMARY KEY (student_id, course_id),
		FOREIGN KEY (student_id) REFERENCES students(id) ON DELETE CASCADE,
		FOREIGN KEY (course_id) REFERENCES courses(id) ON DELETE CASCADE
	);
	`

	if _, err := db.Exec(schemaDDL); err != nil {
		t.Fatalf("Failed to execute test schema DDL: %v", err)
	}

	return db
}

func TestStudentCourseManyToManyCRUD(t *testing.T) {
	db := setupSQLite3TestDB(t)
	defer db.Close()

	ctx := context.Background()

	// -------------------------------------------------------------------------
	// 1. CREATE (Insert Students & Courses)
	// -------------------------------------------------------------------------
	course1 := &models.Course{Code: "CS101", Title: "Intro to Computer Science", CreatedAt: time.Now()}
	course2 := &models.Course{Code: "MATH201", Title: "Linear Algebra", CreatedAt: time.Now()}

	if err := queries.InsertCourse(ctx, db, course1); err != nil {
		t.Fatalf("InsertCourse(CS101) failed: %v", err)
	}
	if err := queries.InsertCourse(ctx, db, course2); err != nil {
		t.Fatalf("InsertCourse(MATH201) failed: %v", err)
	}

	student := &models.Student{
		Name:      "Alice Smith",
		Email:     "alice@university.edu",
		CreatedAt: time.Now(),
	}

	if err := queries.InsertStudent(ctx, db, student); err != nil {
		t.Fatalf("InsertStudent failed: %v", err)
	}

	// Link student to courses via join table
	linkSQL := `INSERT INTO student_courses (student_id, course_id) VALUES (?, ?), (?, ?)`
	if _, err := db.ExecContext(ctx, linkSQL, student.ID, course1.ID, student.ID, course2.ID); err != nil {
		t.Fatalf("Failed to associate student with courses in pivot table: %v", err)
	}

	// -------------------------------------------------------------------------
	// 2. READ (GetByID with ManyToMany Preload)
	// -------------------------------------------------------------------------
	fetchedStudent, err := queries.GetStudentByID(ctx, db, student.ID, queries.Preload(true))
	if err != nil {
		t.Fatalf("GetStudentByID with Preload failed: %v", err)
	}

	if fetchedStudent.Name != student.Name {
		t.Errorf("Expected student name %q, got %q", student.Name, fetchedStudent.Name)
	}

	if len(fetchedStudent.Courses) != 2 {
		t.Fatalf("Expected 2 preloaded courses, got %d", len(fetchedStudent.Courses))
	}

	expectedCodes := map[string]bool{"CS101": false, "MATH201": false}
	for _, c := range fetchedStudent.Courses {
		expectedCodes[c.Code] = true
	}

	for code, found := range expectedCodes {
		if !found {
			t.Errorf("Expected preloaded course %q was not found", code)
		}
	}

	// -------------------------------------------------------------------------
	// 3. READ ALL (FetchAllStudents with Preload)
	// -------------------------------------------------------------------------
	allStudents, err := queries.FetchAllStudents(ctx, db, queries.Preload(true))
	if err != nil {
		t.Fatalf("FetchAllStudents with Preload failed: %v", err)
	}

	if len(allStudents) != 1 {
		t.Fatalf("Expected 1 total student, got %d", len(allStudents))
	}
	if len(allStudents[0].Courses) != 2 {
		t.Errorf("Expected 2 courses preloaded on FetchAllStudents, got %d", len(allStudents[0].Courses))
	}

	// -------------------------------------------------------------------------
	// 4. UPDATE (Update Student Record)
	// -------------------------------------------------------------------------
	student.Name = "Alice Johnson"
	if err := queries.UpdateStudent(ctx, db, student); err != nil {
		t.Fatalf("UpdateStudent failed: %v", err)
	}

	updatedStudent, err := queries.GetStudentByID(ctx, db, student.ID)
	if err != nil {
		t.Fatalf("GetStudentByID post-update failed: %v", err)
	}
	if updatedStudent.Name != "Alice Johnson" {
		t.Errorf("Expected updated name 'Alice Johnson', got %q", updatedStudent.Name)
	}

	// -------------------------------------------------------------------------
	// 5. DELETE (Soft Delete Student)
	// -------------------------------------------------------------------------
	if err := queries.DeleteStudent(ctx, db, student.ID); err != nil {
		t.Fatalf("DeleteStudent failed: %v", err)
	}

	// Verify standard queries exclude soft-deleted records
	exists, err := queries.ExistsStudentByID(ctx, db, student.ID)
	if err != nil {
		t.Fatalf("ExistsStudentByID failed: %v", err)
	}
	if exists {
		t.Errorf("Expected student to be excluded after soft delete")
	}

	// Verify hard-delete permanently purges the record
	if err := queries.DeleteStudent(ctx, db, student.ID, queries.IncludeDeleted(), queries.HardDelete()); err != nil {
		t.Fatalf("HardDelete student failed: %v", err)
	}
}
