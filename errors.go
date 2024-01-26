package enthistory

import (
	"errors"
	"fmt"
)

var (
	// ErrUnsupportedIDType is returned when id type other than string or int is used
	ErrUnsupportedIDType = errors.New("unsupported id type, only int and strings are allowed")

	// ErrUnsupportedType is returned when the object type is not supported
	ErrUnsupportedType = errors.New("unsupported type")

	// ErrNoIDType is returned when the id type cannot be determined from the schema
	ErrNoIDType = errors.New("could not get id type for schema")

	// ErrInvalidSchemaPath is returned when the schema path cannot be determined
	ErrInvalidSchemaPath = errors.New("invalid schema path, unable to find package name in path")
)

func newNoIDTypeError(schemaName string) error {
	return fmt.Errorf("%w: %v", ErrNoIDType, schemaName)
}
