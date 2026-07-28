package models

import "time"

// UserSummaryView represents a read-only database view projecting a subset of user fields.
// Since no field is named "ID" and there is no gorm:"primaryKey" tag,
// query-gen treats this as a keyless model (read-only queries, no Insert/Update/Delete).
type UserSummaryView struct {
	UserID       int64     `gorm:"column:user_id"`
	Name         string    `gorm:"column:name"`
	Email        string    `gorm:"column:email"`
	RegisteredAt time.Time `gorm:"column:registered_at"`
}

// TableName explicitly maps this struct to the database view name.
func (UserSummaryView) TableName() string {
	return "user_summary_views"
}
