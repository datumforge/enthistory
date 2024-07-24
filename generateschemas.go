package enthistory

import (
	"embed"
	"fmt"
	"path/filepath"
	"strings"

	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
	"entgo.io/ent/entc/load"
)

var (
	//go:embed templates/*
	_templates embed.FS
)

type templateInfo struct {
	Schema               *load.Schema
	IDType               string
	SchemaPkg            string
	TableName            string
	SchemaName           string
	Query                bool
	OriginalTableName    string
	WithUpdatedBy        bool
	UpdatedByValueType   string
	WithHistoryTimeIndex bool
	AuthzPolicy          authzPolicyInfo
	AddPolicy            bool
}

// authzPolicyInfo is a struct that holds the object type and id field for the authz policy
type authzPolicyInfo struct {
	Enabled         bool
	ObjectType      string
	IDField         string
	NillableIDField bool
}

var (
	historyTableSuffix = "_history"
)

// shouldGenerate checks if the history schema should be generated for the given schema
func shouldGenerate(schema *load.Schema) bool {
	// check if schema has history annotation
	// history annotation is used to exclude schemas from history tracking
	historyAnnotation, ok := schema.Annotations[annotationName]
	if !ok {
		return true
	}

	// unmarshal the history annotation
	annotations, err := jsonUnmarshalAnnotations(historyAnnotation)
	if err != nil {
		return true
	}

	// check if schema should be excluded from history tracking
	// based on the history annotation
	switch {
	case annotations.Exclude:
		// if explicitly excluded, do not generate history schema
		return false
	case annotations.IsHistory:
		// if schema is a history schema, do not generate history schema
		return false
	default:
		return true
	}
}

// GenerateSchemas generates the history schema for all schemas in the schema path
// this should be called before the entc.Generate call
// so the schemas exist at the time of code generation
func (e *HistoryExtension) GenerateSchemas() error {
	graph, err := entc.LoadGraph(e.config.SchemaPath, &gen.Config{})
	if err != nil {
		return fmt.Errorf("failed loading ent graph: %v", err)
	}

	// loop through all schemas and generate history schema, if needed
	for _, schema := range graph.Schemas {
		if shouldGenerate(schema) {
			if err := generateHistorySchema(schema, e.config, graph.IDType.String()); err != nil {
				return err
			}
		}

	}

	return nil
}

// getTemplateInfo returns the template info for the history schema based on the schema and config
func getTemplateInfo(schema *load.Schema, config *Config, idType string) (*templateInfo, error) {
	pkg, err := getPkgFromSchemaPath(config.SchemaPath)
	if err != nil {
		return nil, err
	}

	info := &templateInfo{
		TableName:         fmt.Sprintf("%v%s", getSchemaTableName(schema), historyTableSuffix),
		OriginalTableName: schema.Name,
		SchemaPkg:         pkg,
		SchemaName:        config.SchemaName,
		Query:             config.Query,
		AuthzPolicy: authzPolicyInfo{
			Enabled: config.AuthzPolicy,
		},
		AddPolicy: !config.FirstRun,
	}

	// setup history time and updated by based on config settings
	// add updated_by fields
	if config.UpdatedBy != nil {
		valueType := config.UpdatedBy.valueType

		if valueType == ValueTypeInt {
			info.UpdatedByValueType = "Int"
		} else if valueType == ValueTypeString {
			info.UpdatedByValueType = "String"
		}

		info.WithUpdatedBy = true
	}

	info.WithHistoryTimeIndex = config.HistoryTimeIndex

	// determine id type used in schema
	info.IDType = getIDType(idType)

	return info, nil
}

// generateHistorySchema creates the history schema based on the original schema
func generateHistorySchema(schema *load.Schema, config *Config, idType string) error {
	info, err := getTemplateInfo(schema, config, idType)
	if err != nil {
		return err
	}

	// Load new base history schema
	historySchema, err := loadHistorySchema(info.IDType)
	if err != nil {
		return err
	}

	if info.WithHistoryTimeIndex {
		historySchema.Indexes = append(historySchema.Indexes, &load.Index{Fields: []string{"history_time"}})
	}

	historyFields := createHistoryFields(schema.Fields)

	// if authz policy is enabled, add the object type and id field to the history schema
	if info.AuthzPolicy.Enabled {
		err := info.getAuthzPolicyInfo(schema)
		if err != nil {
			return err
		}
	}

	// merge the original schema onto the history schema
	historySchema.Name = fmt.Sprintf("%vHistory", schema.Name)
	historySchema.Fields = append(historySchema.Fields, historyFields...)

	info.Schema = historySchema

	// Get path to write new history schema file
	path, err := getHistorySchemaPath(schema, config)
	if err != nil {
		return err
	}

	// execute schemaTemplate at the history schema path
	if err = parseSchemaTemplate(*info, path); err != nil {
		return err
	}

	return nil
}

// getHistorySchemaPath returns the path of the history schemas
func getHistorySchemaPath(schema *load.Schema, config *Config) (string, error) {
	abs, err := filepath.Abs(config.SchemaPath)
	if err != nil {
		return "", err
	}

	path := fmt.Sprintf("%s/%s%s.go", abs, strings.ToLower(schema.Name), historyTableSuffix)

	return path, nil
}

// createHistoryFields sets the fields for the history schema, which should include
// all fields from the original schema as well as fields from the original schema included
// by mixins
func createHistoryFields(schemaFields []*load.Field) []*load.Field {
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

// getAuthzPolicyInfo sets the object type and id field for the authz policy
// based on the schema and takes advantage of the org owned and user owned policies
// TODO: add support for custom authz policies
func (t *templateInfo) getAuthzPolicyInfo(schema *load.Schema) error {
	switch {
	case schema.Name == "Organization", schema.Name == "User":
		t.AuthzPolicy.IDField = "Ref" // this is the original id field
		t.AuthzPolicy.ObjectType = strings.ToLower(schema.Name)
		t.AuthzPolicy.NillableIDField = false

		return nil
	case hasField(schema.Fields, "owner_id"):
		// is it a user owner or organization owner?
		t.AuthzPolicy.IDField = "OwnerID"
		t.AuthzPolicy.ObjectType = "organization"
		t.AuthzPolicy.NillableIDField = true
	case strings.Contains(schema.Name, "Setting"):
		table := strings.TrimSuffix(schema.Name, "Setting")
		t.AuthzPolicy.IDField = fmt.Sprintf("%sID", table)
		t.AuthzPolicy.ObjectType = table
		t.AuthzPolicy.NillableIDField = true
	case hasField(schema.Fields, "organization_id"):
		t.AuthzPolicy.IDField = "OrganizationID"
		t.AuthzPolicy.ObjectType = "organization"
		t.AuthzPolicy.NillableIDField = false

		return nil
	default:
		t.AuthzPolicy.Enabled = false // disable authz policy, we don't have the necessary fields
		return nil
	}

	return nil
}

// hasField checks if a field exists in the schema
func hasField(fields []*load.Field, fieldName string) bool {
	for _, field := range fields {
		if field.Name == fieldName {
			return true
		}
	}

	return false
}
