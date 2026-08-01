package models

import "time"

// Course represents an academic course offered to students.
type Course struct {
	ID        int64      `gorm:"primaryKey;autoIncrement"` // Unique primary key for the course.
	Code      string     `gorm:"not null;uniqueIndex"`     // Unique course code (e.g., CS101).
	Title     string     `gorm:"not null"`                 // Descriptive title of the course.
	CreatedAt time.Time  `gorm:"column:created_at"`        // Timestamp when the course was created.
	DeletedAt *time.Time `gorm:"null;column:deleted_at"`   // Optional soft-delete timestamp.
}

// Student represents a enrolled student enrolled in zero or more courses.
type Student struct {
	ID        int64      `gorm:"primaryKey;autoIncrement"` // Unique primary key for the student.
	Name      string     `gorm:"not null"`                 // Full name of the student.
	Email     string     `gorm:"not null;uniqueIndex"`     // Unique email address.
	CreatedAt time.Time  `gorm:"column:created_at"`        // Timestamp when student record was created.
	DeletedAt *time.Time `gorm:"null;column:deleted_at"`   // Optional soft-delete timestamp.

	// Relationship: ManyToMany joined via student_courses pivot table.
	Courses []Course `gorm:"many2many:student_courses;constraint:OnDelete:CASCADE,OnUpdate:CASCADE;"`
}
