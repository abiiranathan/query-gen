package models

type Order struct {
	ID     int64   `gorm:"primaryKey;column:order_id"`
	UserID int64   `gorm:"column:user_id"`
	Amount float64 `gorm:"column:amount"`

	// Relationship: BelongsTo
	User *User `gorm:"foreignKey:UserID;references:ID"`
}
