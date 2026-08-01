package queries_test

import (
	"errors"
	"testing"
	"time"

	"github.com/abiiranathan/query-gen/example/queries"
)

// customScanner implements sql.Scanner for testing custom scanner delegation.
type customScanner struct {
	Val string
}

// Scan implements the sql.Scanner interface.
func (c *customScanner) Scan(src any) error {
	if src == nil {
		c.Val = ""
		return nil
	}
	s, ok := src.(string)
	if !ok {
		return errors.New("customScanner: expected string source")
	}
	c.Val = "scanned:" + s
	return nil
}

// customStringType tests underlying primitive string types (e.g., type Age string).
type customStringType string

// customIntType tests underlying primitive int types (e.g., type Status int).
type customIntType int

func TestScanNullable_NilSource(t *testing.T) {
	t.Run("nil into string zeroes destination", func(t *testing.T) {
		val := "initial value"
		scanner := queries.ScanNullable(&val)

		if err := scanner.Scan(nil); err != nil {
			t.Fatalf("unexpected error scanning nil: %v", err)
		}
		if val != "" {
			t.Errorf("expected empty string, got %q", val)
		}
	})

	t.Run("nil into int zeroes destination", func(t *testing.T) {
		val := 42
		scanner := queries.ScanNullable(&val)

		if err := scanner.Scan(nil); err != nil {
			t.Fatalf("unexpected error scanning nil: %v", err)
		}
		if val != 0 {
			t.Errorf("expected 0, got %d", val)
		}
	})

	t.Run("nil into pointer type sets pointer to nil", func(t *testing.T) {
		str := "hello"
		ptr := &str
		scanner := queries.ScanNullable(&ptr)

		if err := scanner.Scan(nil); err != nil {
			t.Fatalf("unexpected error scanning nil: %v", err)
		}
		if ptr != nil {
			t.Errorf("expected nil pointer, got %v", ptr)
		}
	})
}

func TestScanNullable_DirectValueScan(t *testing.T) {
	t.Run("direct type assertion for primitives", func(t *testing.T) {
		var str string
		if err := queries.ScanNullable(&str).Scan("hello world"); err != nil {
			t.Fatalf("Scan failed: %v", err)
		}
		if str != "hello world" {
			t.Errorf("expected 'hello world', got %q", str)
		}

		var num int64
		if err := queries.ScanNullable(&num).Scan(int64(100)); err != nil {
			t.Fatalf("Scan failed: %v", err)
		}
		if num != 100 {
			t.Errorf("expected 100, got %d", num)
		}
	})
}

func TestScanNullable_PrimitiveConversions(t *testing.T) {
	tests := []struct {
		name     string
		target   any
		source   any
		validate func(t *testing.T)
	}{
		{
			name:   "[]byte into string",
			target: new(string),
			source: []byte("byte string"),
			validate: func(t *testing.T) {
				// Handled in sub-test below
			},
		},
		{
			name:   "string into []byte",
			target: new([]byte),
			source: "string to bytes",
			validate: func(t *testing.T) {
				// Handled in sub-test below
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = tt
		})
	}

	t.Run("[]byte into string", func(t *testing.T) {
		var s string
		if err := queries.ScanNullable(&s).Scan([]byte("hello bytes")); err != nil {
			t.Fatalf("Scan failed: %v", err)
		}
		if s != "hello bytes" {
			t.Errorf("expected 'hello bytes', got %q", s)
		}
	})

	t.Run("string into []byte", func(t *testing.T) {
		var b []byte
		if err := queries.ScanNullable(&b).Scan("hello string"); err != nil {
			t.Fatalf("Scan failed: %v", err)
		}
		if string(b) != "hello string" {
			t.Errorf("expected 'hello string', got %q", string(b))
		}
	})

	t.Run("int cross-type numeric conversions", func(t *testing.T) {
		var targetInt int32
		if err := queries.ScanNullable(&targetInt).Scan(int64(12345)); err != nil {
			t.Fatalf("Scan failed: %v", err)
		}
		if targetInt != 12345 {
			t.Errorf("expected 12345, got %d", targetInt)
		}

		var targetUint uint64
		if err := queries.ScanNullable(&targetUint).Scan(int(99)); err != nil {
			t.Fatalf("Scan failed: %v", err)
		}
		if targetUint != 99 {
			t.Errorf("expected 99, got %d", targetUint)
		}
	})
}

func TestScanNullable_TimeParsing(t *testing.T) {
	now := time.Now().Truncate(time.Second).UTC()

	t.Run("RFC3339 string into time.Time", func(t *testing.T) {
		var tm time.Time
		strVal := now.Format(time.RFC3339)

		if err := queries.ScanNullable(&tm).Scan(strVal); err != nil {
			t.Fatalf("Scan time failed: %v", err)
		}
		if !tm.Equal(now) {
			t.Errorf("expected %v, got %v", now, tm)
		}
	})

	t.Run("SQL format string into time.Time", func(t *testing.T) {
		var tm time.Time
		sqlTimeStr := "2026-08-01 18:30:42"

		if err := queries.ScanNullable(&tm).Scan(sqlTimeStr); err != nil {
			t.Fatalf("Scan SQL time string failed: %v", err)
		}
		expected := time.Date(2026, 8, 1, 18, 30, 42, 0, time.UTC)
		if !tm.Equal(expected) {
			t.Errorf("expected %v, got %v", expected, tm)
		}
	})

	t.Run("[]byte timestamp into time.Time", func(t *testing.T) {
		var tm time.Time
		byteTime := []byte("2026-08-01")

		if err := queries.ScanNullable(&tm).Scan(byteTime); err != nil {
			t.Fatalf("Scan byte date failed: %v", err)
		}
		expected := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		if !tm.Equal(expected) {
			t.Errorf("expected %v, got %v", expected, tm)
		}
	})
}

func TestScanNullable_PointerIndirectionAllocation(t *testing.T) {
	t.Run("automatically allocates nil inner pointer", func(t *testing.T) {
		var ptr *string // Outer variable is *string, so &ptr is **string
		scanner := queries.ScanNullable(&ptr)

		if err := scanner.Scan("allocated value"); err != nil {
			t.Fatalf("Scan failed into nil pointer: %v", err)
		}
		if ptr == nil {
			t.Fatal("expected pointer to be allocated, got nil")
		}
		if *ptr != "allocated value" {
			t.Errorf("expected 'allocated value', got %q", *ptr)
		}
	})

	t.Run("scans into existing non-nil inner pointer", func(t *testing.T) {
		existing := "old"
		ptr := &existing
		scanner := queries.ScanNullable(&ptr)

		if err := scanner.Scan("new value"); err != nil {
			t.Fatalf("Scan failed: %v", err)
		}
		if *ptr != "new value" {
			t.Errorf("expected 'new value', got %q", *ptr)
		}
	})
}

func TestScanNullable_CustomTypesAndScannerInterface(t *testing.T) {
	t.Run("scans into custom type with sql.Scanner interface", func(t *testing.T) {
		var cs customScanner
		if err := queries.ScanNullable(&cs).Scan("test_data"); err != nil {
			t.Fatalf("Scan failed: %v", err)
		}
		if cs.Val != "scanned:test_data" {
			t.Errorf("expected 'scanned:test_data', got %q", cs.Val)
		}
	})

	t.Run("scans underlying custom primitive types", func(t *testing.T) {
		var customStr customStringType
		if err := queries.ScanNullable(&customStr).Scan("custom string"); err != nil {
			t.Fatalf("Scan custom string failed: %v", err)
		}
		if customStr != "custom string" {
			t.Errorf("expected 'custom string', got %q", customStr)
		}

		var customI customIntType
		if err := queries.ScanNullable(&customI).Scan(int64(77)); err != nil {
			t.Fatalf("Scan custom int failed: %v", err)
		}
		if customI != 77 {
			t.Errorf("expected 77, got %d", customI)
		}
	})
}

func TestScanNullable_ErrorCases(t *testing.T) {
	t.Run("incompatible types return descriptive error", func(t *testing.T) {
		var num int
		err := queries.ScanNullable(&num).Scan("not a number")
		if err == nil {
			t.Fatal("expected error when scanning incompatible string into int, got nil")
		}
	})

	t.Run("invalid timestamp string returns parse error", func(t *testing.T) {
		var tm time.Time
		err := queries.ScanNullable(&tm).Scan("invalid-date-format")
		if err == nil {
			t.Fatal("expected error scanning invalid date string, got nil")
		}
	})
}
