package models

import "time"

type User struct {
	ID        int64     `gorm:"primaryKey;column:user_id"`
	Email     string    `gorm:"column:email_address"`
	Name      string    `gorm:"column:full_name"`
	CreatedAt time.Time `gorm:"column:created_at"`
	Ignored   string    `gorm:"-"`
}
