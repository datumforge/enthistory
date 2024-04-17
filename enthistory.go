package enthistory

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"entgo.io/ent/schema/field"

	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
	"entgo.io/ent/entc/load"
)

var (
	//go:embed templates/*
	_templates embed.FS
)

type UpdatedBy struct {
	key       string
	valueType ValueType
}

type FieldProperties struct {
	Nillable  bool
	Immutable bool
}

type Config struct {
	UpdatedBy        *UpdatedBy
	Auditing         bool
	SchemaPath       string
	SchemaName       string
	FieldProperties  *FieldProperties
	HistoryTimeIndex bool
}

func (c Config) Name() string {
	return "HistoryConfig"
}

// HistoryExtension implements entc.Extension.
type HistoryExtension struct {
	entc.DefaultExtension
	config *Config
}

type ExtensionOption = func(*HistoryExtension)

// WithUpdatedBy sets the key and type for pulling updated_by from the context,
// usually done via a middleware to track which users are making which changes
func WithUpdatedBy(key string, valueType ValueType) ExtensionOption {
	return func(ex *HistoryExtension) {
		ex.config.UpdatedBy = &UpdatedBy{
			key:       key,
			valueType: valueType,
		}
	}
}

// WithAuditing allows you to turn on the code generation for the `.Audit()` method
func WithAuditing() ExtensionOption {
	return func(ex *HistoryExtension) {
		ex.config.Auditing = true
	}
}

// WithSchemaName allows you to set an alternative schema name
// This can be used to set a schema name for multi-schema migrations and SchemaConfig feature
// https://entgo.io/docs/multischema-migrations/
func WithSchemaName(schemaName string) ExtensionOption {
	return func(ex *HistoryExtension) {
		ex.config.SchemaName = schemaName
	}
}

// WithSchemaPath allows you to set an alternative schemaPath
// Defaults to "./schema"
func WithSchemaPath(schemaPath string) ExtensionOption {
	return func(ex *HistoryExtension) {
		ex.config.SchemaPath = schemaPath
	}
}

// WithNillableFields allows you to set all tracked fields in history to Nillable
// except enthistory managed fields (history_time, ref, operation, updated_by, & deleted_by)
func WithNillableFields() ExtensionOption {
	return func(ex *HistoryExtension) {
		ex.config.FieldProperties.Nillable = true
	}
}

// WithImmutableFields allows you to set all tracked fields in history to Immutable
func WithImmutableFields() ExtensionOption {
	return func(ex *HistoryExtension) {
		ex.config.FieldProperties.Immutable = true
	}
}

// WithHistoryTimeIndex allows you to add an index to the "history_time" fields
func WithHistoryTimeIndex() ExtensionOption {
	return func(ex *HistoryExtension) {
		ex.config.HistoryTimeIndex = true
	}
}

// NewHistoryExtension creates a new history extension
func NewHistoryExtension(opts ...ExtensionOption) *HistoryExtension {
	extension := &HistoryExtension{
		// Set configuration defaults that can get overridden with ExtensionOption
		config: &Config{
			SchemaPath:      "./schema",
			Auditing:        false,
			FieldProperties: &FieldProperties{},
		},
	}

	for _, opt := range opts {
		opt(extension)
	}

	return extension
}

type templateInfo struct {
	Schema               *load.Schema
	IDType               string
	SchemaPkg            string
	TableName            string
	SchemaName           string
	OriginalTableName    string
	WithUpdatedBy        bool
	UpdatedByValueType   string
	WithHistoryTimeIndex bool
}

// Templates returns the generated templates which include the client, history query, history from mutation
// and an optional auditing template
func (h *HistoryExtension) Templates() []*gen.Template {
	templates := []*gen.Template{
		parseTemplate("historyFromMutation", "templates/historyFromMutation.tmpl"),
		parseTemplate("historyQuery", "templates/historyQuery.tmpl"),
		parseTemplate("historyClient", "templates/historyClient.tmpl"),
	}

	if h.config.Auditing {
		templates = append(templates, parseTemplate("auditing", "templates/auditing.tmpl"))
	}

	return templates
}

// Hooks of the HistoryExtension.
func (h *HistoryExtension) Hooks() []gen.Hook {
	return []gen.Hook{
		h.generateHistorySchemas,
	}
}

// Annotations of the HistoryExtension
func (h *HistoryExtension) Annotations() []entc.Annotation {
	return []entc.Annotation{
		h.config,
	}
}

var (
	historyTableSuffix = "_history"
)

// generateHistorySchema creates the history schema based on the original schema
func (h *HistoryExtension) generateHistorySchema(schema *load.Schema, idType string) (*load.Schema, error) {
	pkg, err := getPkgFromSchemaPath(h.config.SchemaPath)
	if err != nil {
		return nil, err
	}

	info := templateInfo{
		TableName:         fmt.Sprintf("%v%s", getSchemaTableName(schema), historyTableSuffix),
		OriginalTableName: schema.Name,
		SchemaPkg:         pkg,
		SchemaName:        h.config.SchemaName,
	}

	// setup history time and updated by based on config settings
	if h.config != nil {
		// add updated_by fields
		if h.config.UpdatedBy != nil {
			valueType := h.config.UpdatedBy.valueType

			if valueType == ValueTypeInt {
				info.UpdatedByValueType = "Int"
			} else if valueType == ValueTypeString {
				info.UpdatedByValueType = "String"
			}

			info.WithUpdatedBy = true
		}

		info.WithHistoryTimeIndex = h.config.HistoryTimeIndex
	}

	// determine id type used in schema
	info.IDType = getIDType(idType)

	// Load new base history schema
	historySchema, err := loadHistorySchema(info.IDType)
	if err != nil {
		return nil, err
	}

	if info.WithHistoryTimeIndex {
		historySchema.Indexes = append(historySchema.Indexes, &load.Index{Fields: []string{"history_time"}})
	}

	historyFields := h.createHistoryFields(schema.Fields)

	// merge the original schema onto the history schema
	historySchema.Name = fmt.Sprintf("%vHistory", schema.Name)
	historySchema.Fields = append(historySchema.Fields, historyFields...)
	historySchema.Annotations = map[string]any{
		"EntSQL": map[string]any{
			"table":  info.TableName,
			"schema": info.SchemaName,
		},
		"History": map[string]any{
			"isHistory": true,
			"exclude":   true,
		},
		"DATUM_SCHEMAGEN": map[string]any{
			"skip": true,
		},
	}

	info.Schema = historySchema

	// Get path to write new history schema file
	path, err := h.getHistorySchemaPath(schema)
	if err != nil {
		return nil, err
	}

	// Create history schema file
	create, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	defer create.Close()

	// execute schemaTemplate at the history schema path
	if err = parseSchemaTemplate(create, info); err != nil {
		return nil, err
	}

	return historySchema, nil
}

// generateHistorySchemas removes the hold generated history schemas and returns
// the generate method to create the new set of history schemas based on the annoations
// of existing schemas
func (h *HistoryExtension) generateHistorySchemas(next gen.Generator) gen.Generator {
	return gen.GenerateFunc(func(g *gen.Graph) error {
		if err := h.removeOldGenerated(g.Schemas); err != nil {
			return err
		}

		var schemas []*load.Schema

		for _, schema := range g.Schemas {
			annotations := getHistoryAnnotations(schema)

			if annotations.Exclude {
				if !annotations.IsHistory {
					schemas = append(schemas, schema)
				}

				continue
			}

			var idType *field.TypeInfo

			for _, node := range g.Nodes {
				if schema.Name == node.Name {
					idType = node.ID.Type
				}
			}

			if idType == nil {
				return newNoIDTypeError(schema.Name)
			}

			historySchema, err := h.generateHistorySchema(schema, idType.String())
			if err != nil {
				return err
			}

			// add history schema to list of schemas in the graph
			schemas = append(schemas, schema, historySchema)
		}

		// Create a new graph
		graph, err := gen.NewGraph(g.Config, schemas...)
		if err != nil {
			return err
		}

		return next.Generate(graph)
	})
}

// getHistorySchemaPath returns the path of the history schemas
func (h *HistoryExtension) getHistorySchemaPath(schema *load.Schema) (string, error) {
	abs, err := filepath.Abs(h.config.SchemaPath)
	if err != nil {
		return "", err
	}

	path := fmt.Sprintf("%v/%v.go", abs, fmt.Sprintf("%s%s", strings.ToLower(schema.Name), historyTableSuffix))

	return path, nil
}

// removeOldGenerated removes all existing history schemas (schemas where the annoation has isHistory = true)
// from the path
func (h *HistoryExtension) removeOldGenerated(schemas []*load.Schema) error {
	for _, schema := range schemas {
		path, err := h.getHistorySchemaPath(schema)
		if err != nil {
			return err
		}

		if err = os.RemoveAll(path); err != nil {
			return err
		}
	}

	return nil
}

// createHistoryFields sets the fields for the history schema, which should include
// all fields from the original schema as well as fields from the original schema included
// by mixins
func (h *HistoryExtension) createHistoryFields(schemaFields []*load.Field) []*load.Field {
	historyFields := []*load.Field{}

	// start at 3 because there are three base fields for history tables
	// history_time, ref, and operation
	i := 3

	for _, field := range schemaFields {
		nillable := field.Nillable
		immutable := field.Immutable
		optional := field.Optional

		newField := load.Field{
			Name:         field.Name,
			Info:         copyRef(field.Info),
			Tag:          field.Tag,
			Size:         copyRef(field.Size),
			Enums:        field.Enums,
			Unique:       false,
			Nillable:     nillable,
			Optional:     optional,
			Default:      field.Default,
			DefaultValue: field.DefaultValue,
			DefaultKind:  field.DefaultKind,
			Immutable:    immutable,
			StorageKey:   field.StorageKey,
			Position:     copyRef(field.Position),
			Sensitive:    field.Sensitive,
			SchemaType:   field.SchemaType,
			Annotations:  field.Annotations,
			Comment:      field.Comment,
		}

		// This wipes references to fields from mixins
		// which we want so we don't include anything other than fields
		// from our mixins
		newField.Position = &load.Position{
			Index:      i,
			MixedIn:    false,
			MixinIndex: 0,
		}
		i += 1

		historyFields = append(historyFields, &newField)
	}

	return historyFields
}
