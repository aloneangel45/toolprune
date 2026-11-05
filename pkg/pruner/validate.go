package pruner

import (
	"fmt"
	"regexp"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// ValidationErrorCode classifies schema validation failures.
type ValidationErrorCode string

const (
	ErrInvalidRootType       ValidationErrorCode = "ERR_INVALID_ROOT_TYPE"
	ErrInvalidPropertiesType ValidationErrorCode = "ERR_INVALID_PROPERTIES_TYPE"
	ErrRequiredPropMissing   ValidationErrorCode = "ERR_REQUIRED_PROP_MISSING"
	ErrDuplicateRequired     ValidationErrorCode = "ERR_DUPLICATE_REQUIRED"
	ErrInvalidPropType       ValidationErrorCode = "ERR_INVALID_PROP_TYPE"
	ErrInvalidEnum           ValidationErrorCode = "ERR_INVALID_ENUM"
	ErrInvalidArrayItems     ValidationErrorCode = "ERR_INVALID_ARRAY_ITEMS"
	ErrInvalidRegexPattern   ValidationErrorCode = "ERR_INVALID_REGEX_PATTERN"
)

// ValidationError represents a specific schema defect with JSON Pointer location.
type ValidationError struct {
	Code     ValidationErrorCode `json:"code"`
	Tool     string              `json:"tool"`
	Path     string              `json:"path"`     // JSON Pointer, e.g. "/inputSchema/properties/owner"
	Expected string              `json:"expected"` // What was expected per Draft-07
	Actual   string              `json:"actual"`   // What was found
	Message  string              `json:"message"`  // Human-readable diagnostic
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("[%s] tool=%q path=%q: %s (expected %s, got %s)",
		e.Code, e.Tool, e.Path, e.Message, e.Expected, e.Actual)
}

// ValidationResult holds the outcome of a validation run.
type ValidationResult struct {
	Valid  bool              `json:"valid"`
	Errors []ValidationError `json:"errors,omitempty"`
}

var validDraft07Types = map[string]bool{
	"string":  true,
	"number":  true,
	"integer": true,
	"boolean": true,
	"array":   true,
	"object":  true,
	"null":    true,
}

// ValidateTool checks an individual mcp.Tool against JSON Schema Draft-07 rules.
func ValidateTool(tool mcp.Tool) ValidationResult {
	var errs []ValidationError

	// 1. Root Type check (MUST be "object")
	if tool.InputSchema.Type != "object" {
		errs = append(errs, ValidationError{
			Code:     ErrInvalidRootType,
			Tool:     tool.Name,
			Path:     "/inputSchema/type",
			Expected: `"object"`,
			Actual:   fmt.Sprintf("%q", tool.InputSchema.Type),
			Message:  "Root schema type must be 'object' for MCP tool parameters",
		})
	}

	// 2. Required Array Referential Integrity & Uniqueness
	seenRequired := make(map[string]bool, len(tool.InputSchema.Required))
	for _, req := range tool.InputSchema.Required {
		if seenRequired[req] {
			errs = append(errs, ValidationError{
				Code:     ErrDuplicateRequired,
				Tool:     tool.Name,
				Path:     "/inputSchema/required",
				Expected: "unique property names",
				Actual:   fmt.Sprintf("duplicate %q", req),
				Message:  "Required array contains duplicate property name",
			})
		}
		seenRequired[req] = true

		if tool.InputSchema.Properties == nil {
			errs = append(errs, ValidationError{
				Code:     ErrRequiredPropMissing,
				Tool:     tool.Name,
				Path:     fmt.Sprintf("/inputSchema/required/%s", req),
				Expected: "property defined in /properties",
				Actual:   "properties map is nil",
				Message:  fmt.Sprintf("Required property %q is missing from properties map", req),
			})
		} else if _, exists := tool.InputSchema.Properties[req]; !exists {
			errs = append(errs, ValidationError{
				Code:     ErrRequiredPropMissing,
				Tool:     tool.Name,
				Path:     fmt.Sprintf("/inputSchema/required/%s", req),
				Expected: "property defined in /properties",
				Actual:   "not found in /properties",
				Message:  fmt.Sprintf("Required property %q is missing from properties map", req),
			})
		}
	}

	// 3. Properties validation
	if tool.InputSchema.Properties != nil {
		validateProperties(tool.Name, "/inputSchema/properties", tool.InputSchema.Properties, &errs)
	}

	return ValidationResult{
		Valid:  len(errs) == 0,
		Errors: errs,
	}
}

// validateProperties checks properties recursively.
func validateProperties(toolName, basePath string, props map[string]any, errs *[]ValidationError) {
	for propName, rawProp := range props {
		propPath := fmt.Sprintf("%s/%s", basePath, propName)
		propMap, ok := rawProp.(map[string]any)
		if !ok {
			*errs = append(*errs, ValidationError{
				Code:     ErrInvalidPropertiesType,
				Tool:     toolName,
				Path:     propPath,
				Expected: "schema object map[string]any",
				Actual:   fmt.Sprintf("%T", rawProp),
				Message:  "Property definition must be a valid schema object",
			})
			continue
		}

		// Validate property type
		if rawType, hasType := propMap["type"]; hasType {
			switch t := rawType.(type) {
			case string:
				if !validDraft07Types[t] {
					*errs = append(*errs, ValidationError{
						Code:     ErrInvalidPropType,
						Tool:     toolName,
						Path:     propPath + "/type",
						Expected: "one of: string, number, integer, boolean, array, object, null",
						Actual:   fmt.Sprintf("%q", t),
						Message:  "Invalid JSON Schema Draft-07 type",
					})
				}
				// If array, validate items (Draft-07 allows schema object, boolean schema, or schema array)
				if t == "array" {
					if rawItems, hasItems := propMap["items"]; hasItems {
						switch itemsVal := rawItems.(type) {
						case bool:
							// In Draft-07, a schema can be a boolean (e.g. items: false or items: true)
						case map[string]any:
							if itemType, hasItemType := itemsVal["type"].(string); hasItemType && itemType == "object" {
								if nestedProps, hasNested := itemsVal["properties"].(map[string]any); hasNested {
									validateProperties(toolName, propPath+"/items/properties", nestedProps, errs)
								}
							}
						case []any:
							// Tuple validation (array of schemas)
						default:
							*errs = append(*errs, ValidationError{
								Code:     ErrInvalidArrayItems,
								Tool:     toolName,
								Path:     propPath + "/items",
								Expected: "schema object, boolean, or schema array",
								Actual:   fmt.Sprintf("%T", rawItems),
								Message:  "Array items must be a schema object, boolean schema, or array of schemas",
							})
						}
					}
				}
				// If object with nested properties, recursively validate
				if t == "object" {
					if nestedProps, hasNested := propMap["properties"].(map[string]any); hasNested {
						// Validate nested required referential integrity
						if nr, hasNR := propMap["required"].([]any); hasNR {
							seenNestedReq := make(map[string]bool, len(nr))
							for _, item := range nr {
								if s, ok := item.(string); ok {
									if seenNestedReq[s] {
										*errs = append(*errs, ValidationError{
											Code:     ErrDuplicateRequired,
											Tool:     toolName,
											Path:     propPath + "/required",
											Expected: "unique property names",
											Actual:   fmt.Sprintf("duplicate %q", s),
											Message:  "Required array contains duplicate property name",
										})
									}
									seenNestedReq[s] = true
									if _, exists := nestedProps[s]; !exists {
										*errs = append(*errs, ValidationError{
											Code:     ErrRequiredPropMissing,
											Tool:     toolName,
											Path:     fmt.Sprintf("%s/required/%s", propPath, s),
											Expected: "property defined in nested properties",
											Actual:   "not found in nested properties",
											Message:  fmt.Sprintf("Required property %q is missing from nested properties map", s),
										})
									}
								}
							}
						} else if strList, ok := propMap["required"].([]string); ok {
							seenNestedReq := make(map[string]bool, len(strList))
							for _, s := range strList {
								if seenNestedReq[s] {
									*errs = append(*errs, ValidationError{
										Code:     ErrDuplicateRequired,
										Tool:     toolName,
										Path:     propPath + "/required",
										Expected: "unique property names",
										Actual:   fmt.Sprintf("duplicate %q", s),
										Message:  "Required array contains duplicate property name",
									})
								}
								seenNestedReq[s] = true
								if _, exists := nestedProps[s]; !exists {
									*errs = append(*errs, ValidationError{
										Code:     ErrRequiredPropMissing,
										Tool:     toolName,
										Path:     fmt.Sprintf("%s/required/%s", propPath, s),
										Expected: "property defined in nested properties",
										Actual:   "not found in nested properties",
										Message:  fmt.Sprintf("Required property %q is missing from nested properties map", s),
									})
								}
							}
						}
						validateProperties(toolName, propPath+"/properties", nestedProps, errs)
					}
				}
			case []any:
				for _, item := range t {
					if s, isStr := item.(string); !isStr || !validDraft07Types[s] {
						*errs = append(*errs, ValidationError{
							Code:     ErrInvalidPropType,
							Tool:     toolName,
							Path:     propPath + "/type",
							Expected: "valid Draft-07 type string",
							Actual:   fmt.Sprintf("%v", item),
							Message:  "Invalid multi-type element in JSON Schema Draft-07",
						})
					}
				}
			case []string:
				for _, s := range t {
					if !validDraft07Types[s] {
						*errs = append(*errs, ValidationError{
							Code:     ErrInvalidPropType,
							Tool:     toolName,
							Path:     propPath + "/type",
							Expected: "valid Draft-07 type string",
							Actual:   fmt.Sprintf("%q", s),
							Message:  "Invalid multi-type element in JSON Schema Draft-07",
						})
					}
				}
			default:
				*errs = append(*errs, ValidationError{
					Code:     ErrInvalidPropType,
					Tool:     toolName,
					Path:     propPath + "/type",
					Expected: "string or []any",
					Actual:   fmt.Sprintf("%T", rawType),
					Message:  "Type field must be a string or array of strings",
				})
			}
		}

		// Validate enum
		if rawEnum, hasEnum := propMap["enum"]; hasEnum {
			enumSlice, isSlice := rawEnum.([]any)
			if !isSlice {
				if strSlice, ok := rawEnum.([]string); ok {
					if len(strSlice) == 0 {
						*errs = append(*errs, ValidationError{
							Code:     ErrInvalidEnum,
							Tool:     toolName,
							Path:     propPath + "/enum",
							Expected: "non-empty array of values",
							Actual:   "[]",
							Message:  "Enum must be a non-empty array",
						})
					}
				} else {
					*errs = append(*errs, ValidationError{
						Code:     ErrInvalidEnum,
						Tool:     toolName,
						Path:     propPath + "/enum",
						Expected: "non-empty array of values",
						Actual:   fmt.Sprintf("%v", rawEnum),
						Message:  "Enum must be a non-empty array",
					})
				}
			} else if len(enumSlice) == 0 {
				*errs = append(*errs, ValidationError{
					Code:     ErrInvalidEnum,
					Tool:     toolName,
					Path:     propPath + "/enum",
					Expected: "non-empty array of values",
					Actual:   fmt.Sprintf("%v", rawEnum),
					Message:  "Enum must be a non-empty array",
				})
			}
		}

		// Validate pattern regex
		if rawPattern, hasPattern := propMap["pattern"]; hasPattern {
			if patternStr, isStr := rawPattern.(string); isStr {
				if _, regErr := regexp.Compile(patternStr); regErr != nil {
					*errs = append(*errs, ValidationError{
						Code:     ErrInvalidRegexPattern,
						Tool:     toolName,
						Path:     propPath + "/pattern",
						Expected: "valid regular expression",
						Actual:   patternStr,
						Message:  fmt.Sprintf("Failed to compile regex: %v", regErr),
					})
				}
			}
		}
	}
}

// ValidateTools validates a slice of tools, returning all collected errors.
func ValidateTools(tools []mcp.Tool) ValidationResult {
	var allErrors []ValidationError
	for _, tool := range tools {
		res := ValidateTool(tool)
		allErrors = append(allErrors, res.Errors...)
	}
	return ValidationResult{
		Valid:  len(allErrors) == 0,
		Errors: allErrors,
	}
}
