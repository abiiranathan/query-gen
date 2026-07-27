package perms

import (
	"database/sql/driver"
	"fmt"
	"time"
)

type Permission uint64
type Date time.Time

const (
	Admin Permission = iota
)

// GormDataType returns DATE as data type.
func (Date) GormDataType() string {
	return "DATE"
}

// String is the Date stringer. (YYYY-MM-DD)
func (d Date) String() string {
	t := time.Time(d)
	return t.Format("2006-01-02")
}

// Value implements the driver.Valuer interface for database writes.
func (d Date) Value() (driver.Value, error) {
	t := time.Time(d)
	if t.IsZero() {
		return nil, nil
	}
	return t.Format("2006-01-02"), nil
}

// Scan implements the sql.Scanner interface for database reads.
func (d *Date) Scan(value any) error {
	if value == nil {
		*d = Date{}
		return nil
	}

	switch v := value.(type) {
	case time.Time:
		*d = Date(v)
		return nil
	case string:
		return d.parse(v)
	case []byte:
		return d.parse(string(v))
	default:
		return fmt.Errorf("cannot scan type %T into Date", value)
	}
}

func (d *Date) parse(s string) error {
	if s == "" {
		*d = Date{}
		return nil
	}

	formats := []string{
		"2006-01-02",
		"2006-01-02 15:04:05",
		time.RFC3339,
	}

	for _, layout := range formats {
		if t, err := time.Parse(layout, s); err == nil {
			*d = Date(t)
			return nil
		}
	}

	return fmt.Errorf("failed to parse %q as Date", s)
}
