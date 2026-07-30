package models

import (
	"time"

	"github.com/abiiranathan/query-gen/perms"
)

type Age string

func (Age) DataType() string {
	return "varchar(20)"
}

type User struct {
	ID          int64 `gorm:"primaryKey"`
	Name        string
	Email       string
	CreatedAt   perms.Date // Tests resolution of underlying types for time.Time
	DeletedAt   *time.Time `gorm:"null"`
	Age         Age
	Permissions perms.Permission // Tests resolution of underlying primitive types
	Orders      []Order          `gorm:"foreignKey:UserID;references:ID"`
}

// Tag represents a label attached to a Project.
type Tag struct {
	ID        int64    `gorm:"primaryKey"`
	ProjectID int64    `gorm:"not null;column:project_id"`
	Name      string   `gorm:"not null"`
	Project   *Project `gorm:"foreignKey:ProjectID;constraint:OnDelete:CASCADE,OnUpdate:CASCADE;"`
}

// Category represents a classification group for a Project.
type Category struct {
	ID        int64    `gorm:"primaryKey"`
	ProjectID int64    `gorm:"not null;column:project_id"`
	Name      string   `gorm:"not null"`
	Project   *Project `gorm:"foreignKey:ProjectID;constraint:OnDelete:CASCADE,OnUpdate:CASCADE;"`
}

// Task represents a work item associated with a Project.
type Task struct {
	ID        int64    `gorm:"primaryKey"`
	ProjectID int64    `gorm:"not null;column:project_id"`
	Title     string   `gorm:"not null"`
	Project   *Project `gorm:"foreignKey:ProjectID;constraint:OnDelete:CASCADE,OnUpdate:CASCADE;"`
}

// Project represents a workspace entity containing multiple associated collections.
// Because it has 3 HasMany relations, relation preloading uses separate batched IN queries.
type Project struct {
	ID          int64      `gorm:"primaryKey"`
	Name        string     `gorm:"not null"`
	Description string     `gorm:"column:description"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	DeletedAt   *time.Time `gorm:"null;column:deleted_at"`

	Tags       []Tag      `gorm:"foreignKey:ProjectID"`
	Categories []Category `gorm:"foreignKey:ProjectID"`
	Tasks      []Task     `gorm:"foreignKey:ProjectID"`
}

// UserRole represents a model/junction table with a composite primary key (UserID + RoleID).
type UserRole struct {
	UserID     int64     `gorm:"primaryKey;column:user_id"`
	RoleID     int64     `gorm:"primaryKey;column:role_id"`
	AssignedAt time.Time `gorm:"column:assigned_at"`
}

// TableName returns the explicit table name for the UserRole model.
func (UserRole) TableName() string {
	return "user_roles"
}
