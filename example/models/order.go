package models

type Order struct {
	ID     int64   `gorm:"primaryKey;column:order_id"`
	UserID int64   `gorm:"column:user_id"`
	Amount float64 `gorm:"column:amount"`

	// Relationship: BelongsTo
	User *User `gorm:"foreignKey:UserID;references:ID"`
}

// NonModel struct is not a model and will not be mapped to a database table by GORM.
// It is skipped because it does not have a primary key field.
type NonModel struct {
	Name string
	Item string
}

// SkippedModel struct is a proper model but should be skipped
// because of annotation in the struct comment.
// query-gen: skip
type SkippedModel struct {
	ID   int64  `gorm:"primaryKey;column:id"`
	Name string `gorm:"column:name"`
}
