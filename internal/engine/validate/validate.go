// Package validate coerces and validates tool-call arguments against a tool's
// JSON Schema. Every value reaching Execute has been shape-checked and
// light-coerced (strings→numbers, arrays→singletons) by jsonschema/v6. The
// validator cache avoids re-parsing the same schema bytes across turns.
package validate

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.harness.dev/harness/internal/engine/types"
)

var validatorCache sync.Map

// getSchemaTypes keeps JSON Schema's scalar and array type forms on one coercion path.
func getSchemaTypes(schema map[string]any) []string {
	switch t := schema["type"].(type) {
	case string:
		return []string{t}
	case []any:
		result := make([]string, 0, len(t))
		for _, typ := range t {
			if s, ok := typ.(string); ok {
				result = append(result, s)
			}
		}
		return result
	default:
		return nil
	}
}

// matchesJsonType uses encoding/json's concrete forms so union coercion leaves an already valid branch alone.
func matchesJsonType(value any, typ string) bool {
	switch typ {
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		v, ok := value.(float64)
		return ok && v == math.Trunc(v)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "null":
		return value == nil
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		return false
	}
}

// jsNumber mirrors JavaScript's Number(string) grammar. It differs from Go's
// strconv.ParseFloat: JS accepts 0x/0o/0b integer literals and "Infinity" but
// rejects underscore digit separators and hex-float p-notation. Returns
// (value, ok); ok==false corresponds to JS NaN.
func jsNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true // Number("") === 0 (callers guard non-empty, so unreached)
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			if v, err := strconv.ParseUint(s[2:], base, 64); err == nil {
				return float64(v), true
			}
			return 0, false // e.g. "0x1p4" (hex float) → NaN in JS
		}
	}
	if strings.ContainsRune(s, '_') {
		return 0, false // Go ParseFloat accepts "1_000"; JS → NaN
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// coercePrimitiveByType mirrors JavaScript-style coercion only where the schema asks for it; invalid values stay for validation to explain.
func coercePrimitiveByType(value any, typ string) any {
	switch typ {
	case "number":
		if value == nil {
			return float64(0)
		}
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			if parsed, ok := jsNumber(s); ok && !math.IsInf(parsed, 0) && !math.IsNaN(parsed) {
				return parsed
			}
		}
		if b, ok := value.(bool); ok {
			if b {
				return float64(1)
			}
			return float64(0)
		}
		return value
	case "integer":
		if value == nil {
			return float64(0)
		}
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			if parsed, ok := jsNumber(s); ok && !math.IsInf(parsed, 0) && !math.IsNaN(parsed) && parsed == math.Trunc(parsed) {
				return parsed
			}
		}
		if b, ok := value.(bool); ok {
			if b {
				return float64(1)
			}
			return float64(0)
		}
		return value
	case "boolean":
		if value == nil {
			return false
		}
		if s, ok := value.(string); ok {
			if s == "true" {
				return true
			}
			if s == "false" {
				return false
			}
		}
		if n, ok := value.(float64); ok {
			if n == 1 {
				return true
			}
			if n == 0 {
				return false
			}
		}
		return value
	case "string":
		if value == nil {
			return ""
		}
		if n, ok := value.(float64); ok {
			// deviation: JS String(number) and Go formatting are not byte-for-byte
			// identical for every non-integer; FormatFloat('g', -1) is the closest
			// standard-library equivalent and preserves integer-looking values.
			return strconv.FormatFloat(n, 'g', -1, 64)
		}
		if b, ok := value.(bool); ok {
			return strconv.FormatBool(b)
		}
		return value
	case "null":
		switch v := value.(type) {
		case string:
			if v == "" {
				return nil
			}
		case float64:
			if v == 0 {
				return nil
			}
		case bool:
			if !v {
				return nil
			}
		}
		return value
	default:
		return value
	}
}

// applySchemaObjectCoercion only walks declared or schema-governed extra keys; it doesn't invent missing optional fields.
func applySchemaObjectCoercion(value map[string]any, schema map[string]any) {
	properties, _ := schema["properties"].(map[string]any)
	definedKeys := map[string]bool{}
	for key := range properties {
		definedKeys[key] = true
	}

	for key, propertySchema := range properties {
		if _, ok := value[key]; !ok {
			continue
		}
		if ps, ok := propertySchema.(map[string]any); ok {
			value[key] = coerceWithJSONSchema(value[key], ps)
		}
	}

	additionalProperties, ok := schema["additionalProperties"].(map[string]any)
	if !ok {
		return
	}
	for key, propertyValue := range value {
		if definedKeys[key] {
			continue
		}
		value[key] = coerceWithJSONSchema(propertyValue, additionalProperties)
	}
}

// applySchemaArrayCoercion respects tuple schemas instead of applying a positional rule to every item.
func applySchemaArrayCoercion(value []any, schema map[string]any) {
	switch items := schema["items"].(type) {
	case []any:
		for index := 0; index < len(value); index++ {
			if index >= len(items) || items[index] == nil {
				continue
			}
			if itemSchema, ok := items[index].(map[string]any); ok {
				value[index] = coerceWithJSONSchema(value[index], itemSchema)
			}
		}
	case map[string]any:
		for index := 0; index < len(value); index++ {
			value[index] = coerceWithJSONSchema(value[index], items)
		}
	}
}

// coerceWithUnionSchema isolates each union attempt because object and array coercion mutate their input; the first valid branch wins.
func coerceWithUnionSchema(value any, schemas []map[string]any) any {
	for _, schema := range schemas {
		candidate := deepCopy(value)
		coerced := coerceWithJSONSchema(candidate, schema)
		validator := getSubSchemaValidator(schema)
		if validator != nil && validator.Validate(coerced) == nil {
			return coerced
		}
	}
	return value
}

// coerceWithJSONSchema recursively coerces value toward schema, resolving
// allOf/anyOf/oneOf, primitive type conversions, and nested objects/arrays.
func coerceWithJSONSchema(value any, schema map[string]any) any {
	nextValue := value

	if schemas, ok := schemaList(schema["allOf"]); ok {
		for _, nested := range schemas {
			nextValue = coerceWithJSONSchema(nextValue, nested)
		}
	}

	if schemas, ok := schemaList(schema["anyOf"]); ok {
		nextValue = coerceWithUnionSchema(nextValue, schemas)
	}

	if schemas, ok := schemaList(schema["oneOf"]); ok {
		nextValue = coerceWithUnionSchema(nextValue, schemas)
	}

	schemaTypes := getSchemaTypes(schema)
	matchesUnionMember := len(schemaTypes) > 1
	if matchesUnionMember {
		matchesUnionMember = false
		for _, schemaType := range schemaTypes {
			if matchesJsonType(nextValue, schemaType) {
				matchesUnionMember = true
				break
			}
		}
	}
	if len(schemaTypes) > 0 && !matchesUnionMember {
		for _, schemaType := range schemaTypes {
			candidate := coercePrimitiveByType(nextValue, schemaType)
			if primitiveChanged(candidate, nextValue) {
				nextValue = candidate
				break
			}
		}
	}

	if contains(schemaTypes, "object") {
		if objectValue, ok := nextValue.(map[string]any); ok {
			applySchemaObjectCoercion(objectValue, schema)
		}
	}

	if contains(schemaTypes, "array") {
		if arrayValue, ok := nextValue.([]any); ok {
			applySchemaArrayCoercion(arrayValue, schema)
		}
	}

	return nextValue
}

func schemaList(value any) ([]map[string]any, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if schema, ok := item.(map[string]any); ok {
			result = append(result, schema)
		}
	}
	return result, true
}

// primitiveChanged keeps a no-op conversion from masking a later schema type that can actually coerce the value.
func primitiveChanged(candidate, nextValue any) bool {
	switch candidate := candidate.(type) {
	case nil:
		return nextValue != nil
	case float64:
		v, ok := nextValue.(float64)
		return !ok || candidate != v
	case bool:
		v, ok := nextValue.(bool)
		return !ok || candidate != v
	case string:
		v, ok := nextValue.(string)
		return !ok || candidate != v
	default:
		return false
	}
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// deepCopy protects the caller's decoded arguments while coercion rewrites nested maps and slices.
func deepCopy(value any) any {
	switch v := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(v))
		for key, value := range v {
			clone[key] = deepCopy(value)
		}
		return clone
	case []any:
		clone := make([]any, len(v))
		for index, value := range v {
			clone[index] = deepCopy(value)
		}
		return clone
	default:
		return value
	}
}

// getValidator compiles schema into a validator, caching by its JSON encoding
// so repeated tool calls reuse the same compiled schema.
func getValidator(schema map[string]any) (*jsonschema.Schema, error) {
	key, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	if cached, ok := validatorCache.Load(string(key)); ok {
		return cached.(*jsonschema.Schema), nil
	}

	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("mem://schema", schema); err != nil {
		return nil, err
	}
	validator, err := compiler.Compile("mem://schema")
	if err != nil {
		return nil, err
	}
	actual, _ := validatorCache.LoadOrStore(string(key), validator)
	return actual.(*jsonschema.Schema), nil
}

// getSubSchemaValidator makes an invalid union member ineligible without stopping other coercion attempts.
func getSubSchemaValidator(schema map[string]any) *jsonschema.Schema {
	validator, err := getValidator(schema)
	if err != nil {
		return nil
	}
	return validator
}

// formatValidationPath renders an error's instance location as a dotted path.
// The validator appends the missing property to "required" errors, while
// jsonschema/v6 reports the parent location and names the property in the
// message. Per-error wording may therefore differ.
func formatValidationPath(error *jsonschema.ValidationError) string {
	path := strings.Join(error.InstanceLocation, ".")
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return "root"
	}
	return path
}

// validationErrorLines flattens nested validator causes because only leaves name the input callers can fix.
func validationErrorLines(err error) string {
	var validationError *jsonschema.ValidationError
	if !errors.As(err, &validationError) {
		return fmt.Sprintf("  - root: %v", err)
	}

	leaves := collectValidationLeaves(validationError)
	if len(leaves) == 0 {
		return "Unknown validation error"
	}

	lines := make([]string, 0, len(leaves))
	for _, leaf := range leaves {
		lines = append(lines, fmt.Sprintf("  - %s: %s", formatValidationPath(leaf), validationMessage(leaf)))
	}
	return strings.Join(lines, "\n")
}

// collectValidationLeaves drops grouping nodes so union failures don't print a vague parent error beside its details.
func collectValidationLeaves(error *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if error == nil {
		return nil
	}
	if len(error.Causes) == 0 {
		return []*jsonschema.ValidationError{error}
	}
	var leaves []*jsonschema.ValidationError
	for _, cause := range error.Causes {
		leaves = append(leaves, collectValidationLeaves(cause)...)
	}
	return leaves
}

func validationMessage(error *jsonschema.ValidationError) string {
	output := error.BasicOutput()
	if output != nil && output.Error != nil {
		return output.Error.String()
	}
	return error.Error()
}

// formatValidationError shows pre-coercion arguments so callers can see what the model actually sent.
func formatValidationError(toolCall types.ContentBlock, validationErr error, originalArgs any) error {
	pretty, err := json.MarshalIndent(originalArgs, "", "  ")
	if err != nil {
		pretty = []byte("null")
	}
	// Use raw interpolation (not %q) so quotes and newlines in the tool name
	// remain unescaped.
	return fmt.Errorf("Validation failed for tool \"%s\":\n%s\n\nReceived arguments:\n%s", toolCall.Name, validationErrorLines(validationErr), string(pretty))
}

// ValidateToolArguments coerces and validates a tool call's arguments against
// the tool's JSON Schema parameters, returning the validated JSON arguments.
func ValidateToolArguments(tool types.Tool, toolCall types.ContentBlock) (json.RawMessage, error) {
	var args any
	if err := json.Unmarshal(toolCall.Arguments, &args); err != nil {
		return nil, err
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
		return nil, err
	}

	coerced := coerceWithJSONSchema(deepCopy(args), schema)
	validator, err := getValidator(schema)
	if err != nil {
		return nil, err
	}
	// Top-level tool arguments are always objects, so an unreachable primitive
	// coercion branch is intentionally omitted.
	if err := validator.Validate(coerced); err != nil {
		// jsonschema/v6 supplies a nested ValidationError tree. The outer error
		// structure is preserved while per-leaf wording comes from jsonschema/v6.
		return nil, formatValidationError(toolCall, err, args)
	}

	validated, err := json.Marshal(coerced)
	if err != nil {
		return nil, err
	}
	return validated, nil
}

// ValidateToolCall finds a tool by name and validates the tool call arguments.
func ValidateToolCall(tools []types.Tool, toolCall types.ContentBlock) (json.RawMessage, error) {
	for _, tool := range tools {
		if tool.Name == toolCall.Name {
			return ValidateToolArguments(tool, toolCall)
		}
	}
	return nil, fmt.Errorf("Tool \"%s\" not found", toolCall.Name)
}
