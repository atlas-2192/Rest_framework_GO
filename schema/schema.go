package schema

import (
	"context"
	"fmt"
	"log"
	"reflect"
)

type internal struct{}

// Tombstone is used to mark a field for removal.
var Tombstone = internal{}

// Validator is an interface used to validate schema against actual data.
type Validator interface {
	GetField(name string) *Field
	Prepare(ctx context.Context, payload map[string]interface{}, original *map[string]interface{}, replace bool) (changes map[string]interface{}, base map[string]interface{})
	Validate(changes map[string]interface{}, base map[string]interface{}) (doc map[string]interface{}, errs map[string][]interface{})
}

// Schema defines fields for a document.
type Schema struct {
	// Description of the object described by this schema.
	Description string
	// Fields defines the schema's allowed fields.
	Fields Fields
	// MinLen defines the minimum number of fields (default 0).
	MinLen int
	// MaxLen defines the maximum number of fields (default no limit).
	MaxLen int
}

// Compile implements the ReferenceCompiler interface and call the same function
// on each field. Note: if you use schema as a standalone library, it is the
// *caller's* responsibility to invoke the Compile method before using Prepare
// or Validate on a Schema instance, otherwise FieldValidator instances may not
// be initialized correctly.
func (s Schema) Compile(rc ReferenceChecker) error {
	if err := compileDependencies(s, s); err != nil {
		return err
	}
	for field, def := range s.Fields {
		// Compile each field.
		if err := def.Compile(rc); err != nil {
			return fmt.Errorf("%s%v", field, err)
		}
	}
	return nil
}

// GetField implements the FieldGetter interface.
func (s Schema) GetField(name string) *Field {
	name, remaining, wasSplit := splitFieldPath(name)

	field, found := s.Fields[name]

	if !found {
		// invalid name.
		return nil
	}

	if !wasSplit {
		// no remaining, return field.
		return &field
	}

	if field.Schema != nil {
		// Recursively call GetField to consume whole path.
		// TODO: This will be removed when implementing issue #77.
		return field.Schema.GetField(remaining)
	}

	if fg, ok := field.Validator.(FieldGetter); ok {
		// Recursively call GetField to consume whole path.
		return fg.GetField(remaining)
	}

	return nil
}

// Prepare takes a payload with an optional original payout when updating an
// existing item and return two maps, one containing changes operated by the
// user and another defining either existing data (from the current item) or
// data generated by the system thru "default" value or hooks.
//
// If the original map is nil, prepare will act as if the payload is a new
// document. The OnInit hook is executed for each field if any, and default
// values are assigned to missing fields.
//
// When the original map is defined, the payload is considered as an update on
// the original document, default values are not assigned, and only fields which
// are different than in the original are left in the change map. The OnUpdate
// hook is executed on each field.
//
// If the replace argument is set to true with the original document set, the
// behavior is slightly different as any field not present in the payload but
// present in the original are set to nil in the change map (instead of just
// being absent). This instruct the validator that the field has been edited, so
// ReadOnly flag can throw an error and the field will be removed from the
// output document. The OnInit is also called instead of the OnUpdate.
func (s Schema) Prepare(ctx context.Context, payload map[string]interface{}, original *map[string]interface{}, replace bool) (changes map[string]interface{}, base map[string]interface{}) {
	changes = map[string]interface{}{}
	base = map[string]interface{}{}
	for field, def := range s.Fields {
		value, found := payload[field]
		if original == nil {
			if replace == true {
				log.Panic("Cannot use replace=true without original")
			}
			// Handle prepare on a new document (no original).
			if !found || value == nil {
				// Add default fields
				if def.Default != nil {
					base[field] = def.Default
				}
			} else if found {
				changes[field] = value
			}
		} else {
			// Handle prepare on an updated document (original provided).
			oValue, oFound := (*original)[field]
			// Apply value to change-set only if the field was not identical same in the original doc.
			if found {
				if def.Validator != nil {
					if validated, err := def.Validator.Validate(value); err != nil {
						// We treat a validation error as a change; the validation
						// error indicate invalid payload and will be caught
						// again by schema.Validate().
						changes[field] = value
					} else if !oFound || !reflect.DeepEqual(validated, oValue) {
						changes[field] = validated
					}
				} else if !oFound || !reflect.DeepEqual(value, oValue) {
					changes[field] = value
				}
			} else if oFound && replace {
				// When replace arg is true and a field is not present in the payload but is in the original,
				// the tombstone value is set on the field in the change map so validator can enforce the
				// ReadOnly and then the field can be removed from the output document.
				// One exception to that though: if the field is set to hidden and is not readonly, we use
				// previous value as the client would have no way to resubmit the stored value.
				if def.Hidden && !def.ReadOnly {
					changes[field] = oValue
				} else if def.Required && def.Default != nil {
					changes[field] = def.Default
				} else {
					changes[field] = Tombstone
				}
			}
			if oFound {
				base[field] = oValue
			}
		}
		if def.Schema != nil {
			// Prepare sub-schema
			var subOriginal *map[string]interface{}
			if original != nil {
				// If original is provided, prepare the sub field if it exists and
				// is a dictionary. Otherwise, use an empty dict.
				oValue := (*original)[field]
				subOriginal = &map[string]interface{}{}
				if su, ok := oValue.(*map[string]interface{}); ok {
					subOriginal = su
				}
			}
			if found {
				if subPayload, ok := value.(map[string]interface{}); ok {
					// If payload contains a sub-document for this field, validate it
					// using the sub-validator.
					c, b := def.Schema.Prepare(ctx, subPayload, subOriginal, replace)
					changes[field] = c
					base[field] = b
				} else {
					// Invalid payload, it will be caught by Validate().
				}
			} else {
				// If the payload doesn't contain a sub-document, perform validation
				// on an empty one so we don't miss default values.
				c, b := def.Schema.Prepare(ctx, map[string]interface{}{}, subOriginal, replace)
				if len(c) > 0 || len(b) > 0 {
					// Only apply prepared field if something was added.
					changes[field] = c
					base[field] = b
				}
			}
		}
		// Call the OnInit or OnUpdate depending on the presence of the original doc and the
		// state of the replace argument.
		var hook func(ctx context.Context, value interface{}) interface{}
		if original == nil {
			hook = def.OnInit
		} else {
			hook = def.OnUpdate
		}
		if hook != nil {
			// Get the change value or fallback on the base value.
			if value, found := changes[field]; found {
				if value == Tombstone {
					// If the field has a tombstone, apply the handler on the
					// base and remove the tombstone so it doesn't appear as a
					// user generated change.
					base[field] = hook(ctx, base[field])
					delete(changes, field)
				} else {
					changes[field] = hook(ctx, value)
				}
			} else {
				base[field] = hook(ctx, base[field])
			}
		}
	}
	// Assign all out of schema fields to the changes map so Validate() can
	// complain about it.
	for field, value := range payload {
		if _, found := s.Fields[field]; !found {
			changes[field] = value
		}
	}
	return
}

// Validate validates changes applied on a base document in regard to the schema
// and generate an result document with the changes applied to the base document.
// All errors in the process are reported in the returned errs value.
func (s Schema) Validate(changes map[string]interface{}, base map[string]interface{}) (doc map[string]interface{}, errs map[string][]interface{}) {
	return s.validate(changes, base, true)
}

func (s Schema) validate(changes map[string]interface{}, base map[string]interface{}, isRoot bool) (doc map[string]interface{}, errs map[string][]interface{}) {
	doc = map[string]interface{}{}
	errs = map[string][]interface{}{}
	for field, def := range s.Fields {
		// Check read only fields.
		if def.ReadOnly {
			if _, found := changes[field]; found {
				addFieldError(errs, field, "read-only")
			}
		}
		// Check required fields.
		if def.Required {
			if value, found := changes[field]; !found || value == nil || value == Tombstone {
				if found {
					// If explicitly set to null, raise the required error.
					addFieldError(errs, field, "required")
				} else if value, found = base[field]; !found || value == nil {
					// If field was omitted and isn't set by a Default of a hook, raise.
					addFieldError(errs, field, "required")
				}
			}
		}
		// Validate sub-schema on non provided fields in order to enforce
		// required.
		if def.Schema != nil {
			if _, found := changes[field]; !found {
				if _, found := base[field]; !found {
					empty := map[string]interface{}{}
					if _, subErrs := def.Schema.validate(empty, empty, false); len(subErrs) > 0 {
						addFieldError(errs, field, subErrs)
					}
				}
			}
		}
	}
	// Apply changes to the base in doc
	for field, value := range base {
		doc[field] = value
	}
	for field, value := range changes {
		if value == Tombstone {
			// If the value is set for removal, remove it from the doc.
			delete(doc, field)
		} else {
			doc[field] = value
		}
	}
	// Validate all dependency from the root schema only as dependencies can
	// refers to parent schemas.
	if isRoot {
		mergeErrs := s.validateDependencies(changes, doc, "")
		mergeFieldErrors(errs, mergeErrs)
	}
	for field, value := range doc {
		// Check invalid field (fields provided in the payload by not present in
		// the schema).
		def, found := s.Fields[field]
		if !found {
			addFieldError(errs, field, "invalid field")
			continue
		}
		if def.Schema != nil {
			// Schema defines a sub-schema.
			subChanges := map[string]interface{}{}
			subBase := map[string]interface{}{}
			// Check if changes contains a valid sub-document.
			if v, found := changes[field]; found {
				if m, ok := v.(map[string]interface{}); ok {
					subChanges = m
				} else {
					addFieldError(errs, field, "not a dict")
				}
			}
			// Check if base contains a valid sub-document.
			if v, found := base[field]; found {
				if m, ok := v.(map[string]interface{}); ok {
					subBase = m
				} else {
					addFieldError(errs, field, "not a dict")
				}
			}
			// Validate sub document and add the result to the current doc's field.
			if subDoc, subErrs := def.Schema.validate(subChanges, subBase, false); len(subErrs) > 0 {
				addFieldError(errs, field, subErrs)
			} else {
				doc[field] = subDoc
			}
		} else if def.Validator != nil {
			// Apply validator if provided.
			var err error
			if value, err = def.Validator.Validate(value); err != nil {
				addFieldError(errs, field, err.Error())
			} else {
				// Store the normalized value.
				doc[field] = value
			}
		}
	}
	l := len(doc)
	if l < s.MinLen {
		addFieldError(errs, "", fmt.Sprintf("has fewer properties than %d", s.MinLen))
		return nil, errs
	}
	if s.MaxLen > 0 && l > s.MaxLen {
		addFieldError(errs, "", fmt.Sprintf("has more properties than %d", s.MaxLen))
		return nil, errs
	}
	return doc, errs
}

func addFieldError(errs map[string][]interface{}, field string, err interface{}) {
	errs[field] = append(errs[field], err)
}

func mergeFieldErrors(errs map[string][]interface{}, mergeErrs map[string][]interface{}) {
	// TODO recursive merge
	for field, values := range mergeErrs {
		errs[field] = append(errs[field], values...)
	}
}
