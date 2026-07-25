package models

import "time"

type User struct {
	ID        int64 `gorm:"primaryKey"`
	Name      string
	Email     string
	CreatedAt *time.Time
	Orders    []Order `gorm:"foreignKey:UserID;references:ID"`
	DeletedAt *time.Time
}
