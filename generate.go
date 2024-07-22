package enthistory

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"entgo.io/ent/schema/field"

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
		Query:             h.config.Query,
		AuthzPolicy: authzPolicyInfo{
			Enabled: h.config.AuthzPolicy,
		},
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

	// if authz policy is enabled, add the object type and id field to the history schema
	if info.AuthzPolicy.Enabled {
		err := info.getAuthzPolicyInfo(schema)
		if err != nil {
			return nil, err
		}
	}

	// merge the original schema onto the history schema
	historySchema.Name = fmt.Sprintf("%vHistory", schema.Name)
	historySchema.Fields = append(historySchema.Fields, historyFields...)

	// annotations for the history schema need to be added here, in addition to the schema
	// because they are loaded in memory and decisions in the schema are made based on the annotations
	// before the actual schema is written to disk
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
		"Authz": map[string]any{
			"ObjectType":   info.AuthzPolicy.ObjectType,
			"IDField":      info.AuthzPolicy.IDField,
			"IncludeHooks": false,
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
// the generate method to create the new set of history schemas based on the annotations
// of existing schemas
func (h *HistoryExtension) generateHistorySchemas(next gen.Generator) gen.Generator {
	return gen.GenerateFunc(func(g *gen.Graph) error {
		// Create history schemas concurrently
		var wg sync.WaitGroup

		for _, schema := range g.Schemas {
			wg.Add(1)
			go h.createSchemas(g, schema, &wg)
		}

		wg.Wait()

		// Create a new graph
		graph, err := gen.NewGraph(g.Config, h.schemas...)
		if err != nil {
			return err
		}

		return next.Generate(graph)
	})
}

// createSchemas creates the history schema for the schema and adds it to the list of schemas
func (h *HistoryExtension) createSchemas(g *gen.Graph, schema *load.Schema, wg *sync.WaitGroup) {
	defer wg.Done()

	annotations := getHistoryAnnotations(schema)

	if annotations.Exclude {
		if !annotations.IsHistory {
			h.schemas = append(h.schemas, schema)
		}

		return
	}

	var idType *field.TypeInfo

	for _, node := range g.Nodes {
		if schema.Name == node.Name {
			idType = node.ID.Type
		}
	}

	if idType == nil {
		panic(newNoIDTypeError(schema.Name))
	}

	historySchema, err := h.generateHistorySchema(schema, idType.String())
	if err != nil {
		panic(err)
	}

	// add history schema to list of schemas in the graph
	h.schemas = append(h.schemas, schema, historySchema)

	// sort schemas alphabetically
	h.schemas = sortSchemasAlphabetically(h.schemas)
}

// getHistorySchemaPath returns the path of the history schemas
func (h *HistoryExtension) getHistorySchemaPath(schema *load.Schema) (string, error) {
	abs, err := filepath.Abs(h.config.SchemaPath)
	if err != nil {
		return "", err
	}

	path := fmt.Sprintf("%s/%s%s.go", abs, strings.ToLower(schema.Name), historyTableSuffix)

	return path, nil
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

// if organization -> use id field
// if org owned --> OwnerId is the field to use
// if has field organization_id, use that
// if user -> use id field + user type
// if user owned -> use ownerID field
// else -> no permissions
func (t *templateInfo) getAuthzPolicyInfo(schema *load.Schema) error {
	switch {
	case schema.Name == "Organization", schema.Name == "User":
		t.AuthzPolicy.IDField = "Ref" // this is the original id field
		t.AuthzPolicy.ObjectType = strings.ToLower(schema.Name)
		t.AuthzPolicy.NillableIDField = false

		return nil
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
	case hasField(schema.Fields, "owner_id"):
		// is it a user owner or organization owner?
		t.AuthzPolicy.IDField = "OwnerID"
		t.AuthzPolicy.ObjectType = "organization"
		t.AuthzPolicy.NillableIDField = true
	default:
		fmt.Println("we got nothing for:", schema.Name)
		t.AuthzPolicy.Enabled = false // disable authz policy
		return nil                    // no permissions
	}

	return nil
}

func hasField(fields []*load.Field, fieldName string) bool {
	for _, field := range fields {
		if field.Name == fieldName {
			return true
		}
	}

	return false
}

// sortSchemasAlphabetically sorts the schemas alphabetically by name to ensure ordering is consistent
func sortSchemasAlphabetically(schemas []*load.Schema) []*load.Schema {
	// sort schemas alphabetically
	sort.Slice(schemas, func(i, j int) bool {
		return schemas[i].Name < schemas[j].Name
	})

	return schemas
}
