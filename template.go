package enthistory

import (
	"os"
	"strings"
	"text/template"

	"entgo.io/ent/entc/gen"
	"github.com/stoewer/go-strcase"
)

// extractUpdatedByKey gets the key that is used for the updated_by field
func extractUpdatedByKey(val any) string {
	updatedBy, ok := val.(*UpdatedBy)
	if !ok || updatedBy == nil {
		return ""
	}

	return updatedBy.key
}

// extractUpdatedByValueType gets the type (int or string) that the update_by
// field uses
func extractUpdatedByValueType(val any) string {
	updatedBy, ok := val.(*UpdatedBy)
	if !ok || updatedBy == nil {
		return ""
	}

	switch updatedBy.valueType {
	case ValueTypeInt:
		return "int"
	case ValueTypeString:
		return "string"
	default:
		return ""
	}
}

// fieldPropertiesNillable checks the config properties for the Nillable setting
func fieldPropertiesNillable(config Config) bool {
	return config.FieldProperties != nil && config.FieldProperties.Nillable
}

// isSlice checks if the string value of the type is prefixed with []
func isSlice(typeString string) bool {
	return strings.HasPrefix(typeString, "[]")
}

// in checks a string slice of a given string and returns true, if found
func in(str string, list []string) bool {
	for _, item := range list {
		if item == str {
			return true
		}
	}

	return false
}

// parseTemplate parses the template and sets values in the template
func parseTemplate(name, path string) *gen.Template {
	t := gen.NewTemplate(name)
	t.Funcs(template.FuncMap{
		"extractUpdatedByKey":       extractUpdatedByKey,
		"extractUpdatedByValueType": extractUpdatedByValueType,
		"fieldPropertiesNillable":   fieldPropertiesNillable,
		"isSlice":                   isSlice,
		"in":                        in,
	})

	return gen.MustParse(t.ParseFS(_templates, path))
}

// parseSchemaTemplate parses the template and sets values in the template
func parseSchemaTemplate(create *os.File, info templateInfo) error {
	t := template.New("schema")
	t.Funcs(template.FuncMap{
		"ToUpperCamel": strcase.UpperCamelCase,
		"ToLower":      strings.ToLower,
	})

	template.Must(t.ParseFS(_templates, "templates/schema.tmpl"))

	return t.ExecuteTemplate(create, "schema.tmpl", info)
}
