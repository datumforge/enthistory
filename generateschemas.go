package enthistory

import (
	"embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"entgo.io/ent"
	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
	"entgo.io/ent/entc/load"
	"github.com/datumforge/fgax/entfga"
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
	AllowedRelation string
	NillableIDField bool
	OrgOwned        bool
	UserOwned       bool
	SchemaPolicy    ent.Policy
}

var (
	historyTableSuffix = "_history"
)

// GenerateSchemas generates the history schema for all schemas in the schema path
// this should be called before the entc.Generate call
// so the schemas exist at the time of code generation
func (h *HistoryExtension) GenerateSchemas() error {
	graph, err := entc.LoadGraph(h.config.SchemaPath, &gen.Config{})
	if err != nil {
		return fmt.Errorf("%w: failed loading ent graph: %v", ErrFailedToGenerateTemplate, err)
	}

	// loop through all schemas and generate history schema, if needed
	for _, schema := range graph.Schemas {
		if shouldGenerate(schema) {
			if err := generateHistorySchema(schema, h.config, graph.IDType.String()); err != nil {
				return err
			}
		}
	}

	return nil
}

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
			Enabled:         config.Auth.Enabled,
			AllowedRelation: config.Auth.AllowedRelation,
		},
		AddPolicy: !config.Auth.FirstRun,
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
// based on the original schema annotations
func (t *templateInfo) getAuthzPolicyInfo(schema *load.Schema) error {
	// get entfga annotation, if its not found the history schema should not have an authz policy
	annotations, err := getAuthzAnnotation(schema)
	if err != nil {
		// if the schema does not have an authz annotation, and no existing policy, disable the authz policy
		if schema.Policy == nil {
			t.AuthzPolicy.Enabled = false
		}

		// if the schema does not have an authz annotation, but has a policy, do not disable but return
		return nil
	}

	t.AuthzPolicy.NillableIDField = annotations.NillableIDField

	// default to schema name if object type is not set
	if annotations.ObjectType == "" {
		t.AuthzPolicy.ObjectType = strings.ToLower(schema.Name)
	} else {
		t.AuthzPolicy.ObjectType = annotations.ObjectType
	}

	// the id is now the `ref` field on the history table
	if annotations.IDField == "" || annotations.IDField == "ID" {
		t.AuthzPolicy.IDField = "Ref"
	} else {
		t.AuthzPolicy.IDField = annotations.IDField
	}

	t.AuthzPolicy.OrgOwned = isOrgOwned(schema)
	t.AuthzPolicy.UserOwned = isUserOwned(schema)

	return nil
}

// isOrgOwned checks if the schema is org owned and returns true if it is
func isOrgOwned(schema *load.Schema) bool {
	for _, f := range schema.Fields {
		// all org owned objects are mixed in
		if !f.Position.MixedIn {
			continue
		}

		if f.Name == "owner_id" {
			return strings.Contains(f.Comment, "organization")
		}
	}
	return false
}

// isUserOwned checks if the schema is user owned and returns true if it is
func isUserOwned(schema *load.Schema) bool {
	for _, f := range schema.Fields {
		// all org owned objects are mixed in
		if !f.Position.MixedIn {
			continue
		}

		if f.Name == "owner_id" {
			return strings.Contains(f.Comment, "user")
		}
	}
	return false
}

// getAuthzAnnotation looks for the entfga Authz annotation in the schema
// and unmarshals the annotations
func getAuthzAnnotation(schema *load.Schema) (a entfga.Annotations, err error) {
	authzAnnotation, ok := schema.Annotations["Authz"]
	if !ok {
		return a, fmt.Errorf("authz annotation not found in schema %s", schema.Name)
	}

	out, err := json.Marshal(authzAnnotation)
	if err != nil {
		return
	}

	if err = json.Unmarshal(out, &a); err != nil {
		return
	}

	return
}
