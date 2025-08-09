package client

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/mapper"

	"github.com/go-playground/validator/v10"
	"github.com/ovn-kubernetes/libovsdb/model"
)

// global validator instance
// Validator is designed to be thread-safe and used as a singleton instance. https://pkg.go.dev/github.com/go-playground/validator/v10#hdr-Singleton
var validate *validator.Validate

func init() {
	validate = validator.New(validator.WithRequiredStructEnabled())
	// Register custom validations if needed in the future
	// e.g., validate.RegisterValidation("custom_tag", customValidationFunc)
}

// formatValidationErrors formats validator.ValidationErrors into a detailed human-readable string
func formatValidationErrors(modelName string, context string, validationErrs validator.ValidationErrors) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("validation error for model %s", modelName))

	// Append context if provided (e.g., "mutation on column X")
	if context != "" {
		sb.WriteString(fmt.Sprintf(": %s", context))
	}

	if len(validationErrs) > 0 {
		sb.WriteString("; details: [")
		var fieldErrorMessages []string
		for _, fe := range validationErrs {
			targetField := fe.Namespace() // e.g., "Model.Field" or "Model.Nested.Field"
			// For validate.Var on simple type, Namespace might be empty.
			if targetField == "" {
				targetField = fe.Field() // Fallback to field name if any
			}
			if targetField == "" { // If still empty, use a generic term
				targetField = "<value>"
			}

			errMsg := fmt.Sprintf("field '%s' (value: '%v') failed on rule '%s'", targetField, fe.Value(), fe.ActualTag())
			if fe.Param() != "" {
				errMsg += fmt.Sprintf(" (param: %s)", fe.Param())
			}
			fieldErrorMessages = append(fieldErrorMessages, errMsg)
		}
		sb.WriteString(strings.Join(fieldErrorMessages, ", "))
		sb.WriteString("]")
	}
	return sb.String()
}

// validateModel performs validation on a given model struct using its tags.
func validateModel(m model.Model) error {
	if m == nil {
		return fmt.Errorf("model cannot be nil")
	}

	// Perform the validation
	err := validate.Struct(m)
	if err != nil {
		modelType := reflect.TypeOf(m).Elem()
		modelNameStr := modelType.String()
		var validationErrs validator.ValidationErrors
		if errors.As(err, &validationErrs) {
			formattedErr := formatValidationErrors(modelNameStr, "", validationErrs)
			return fmt.Errorf("model validation failed: %s: %w", formattedErr, validationErrs)
		}
		return fmt.Errorf("error while validating model of type %s: %w", modelNameStr, err)
	}
	return nil
}

// validateMutations performs validation on a given slice of mutations.
func validateMutations(model model.Model, info *mapper.Info, mutations ...model.Mutation) error {
	modelType := reflect.TypeOf(model).Elem()
	modelNameStr := modelType.String()

	for _, mutation := range mutations {
		columnName, err := info.ColumnByPtr(mutation.Field)
		if err != nil {
			return fmt.Errorf("could not get column for mutation field: %w", err)
		}
		// Find the struct field corresponding to the column name
		var structField reflect.StructField
		var found bool
		for i := 0; i < modelType.NumField(); i++ {
			if modelType.Field(i).Tag.Get("ovsdb") == columnName {
				structField = modelType.Field(i)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("could not find struct field for column %s", columnName)
		}

		// Extract the validate tag
		validateTag := structField.Tag.Get("validate")

		// Validate the mutation value if a tag exists
		if validateTag != "" {
			err = validate.Var(mutation.Value, validateTag)
			if err != nil {
				var validationErrs validator.ValidationErrors
				if errors.As(err, &validationErrs) {
					context := fmt.Sprintf("mutation on column %s", columnName)
					formattedErr := formatValidationErrors(modelNameStr, context, validationErrs)
					return fmt.Errorf("mutation validation failed: %s: %w", formattedErr, validationErrs)
				}
				return fmt.Errorf("error while validating mutation for model of type %s on column %s: %w", modelNameStr, columnName, err)
			}
		}
	}
	return nil
}
