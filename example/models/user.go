package models

import "time"

type Age string

func (Age) DataType() string {
	return "varchar(20)"
}

type User struct {
	ID        int64 `gorm:"primaryKey"`
	Name      string
	Email     string
	CreatedAt time.Time
	DeletedAt *time.Time `gorm:"null"`
	Age       Age

	Orders []Order `gorm:"foreignKey:UserID;references:ID"`
}
