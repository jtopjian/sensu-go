package dynamic

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	jsoniter "github.com/json-iterator/go"
)

// AttrGetter is required to be implemented by types that need to work with
// GetField and Marshal.
type AttrGetter interface {
	GetExtendedAttributes() []byte
}

// AttrSetter is required to be implemented by types that need to work with
// Unmarshal.
type AttrSetter interface {
	SetExtendedAttributes([]byte)
}

// GetField gets a field from v according to its name.
// If GetField doesn't find a struct field with the corresponding name, then
// it will try to dynamically find the corresponding item in the 'Extended'
// field. GetField is case-sensitive, but extended attribute names will be
// converted to CamelCaps.
func GetField(v AttrGetter, name string) (interface{}, error) {
	if len(name) == 0 {
		return nil, errors.New("dynamic: empty path specified")
	}
	extendedAttributes := v.GetExtendedAttributes()
	extAttrPtr := &extendedAttributes[0]
	if s := string([]rune(name)[0]); strings.Title(s) == s {
		// Exported fields are always upper-cased for the first rune
		strukt := reflect.Indirect(reflect.ValueOf(v))
		if kind := strukt.Kind(); kind != reflect.Struct {
			return nil, fmt.Errorf("invalid type (want struct): %v", kind)
		}
		field := strukt.FieldByName(name)
		if field.IsValid() {
			rval := reflect.Indirect(field).Interface()
			if b, ok := rval.([]byte); ok && len(b) > 0 {
				// Make sure this field isn't the extended attributes
				if extAttrPtr == &b[0] {
					goto EXTENDED
				}
			}
			return rval, nil
		}
	}
EXTENDED:
	// If we get here, we are dealing with extended attributes.
	any := AnyParameters{any: jsoniter.Get(extendedAttributes)}
	return any.Get(name)
}

// AnyParameters connects jsoniter.Any to govaluate.Parameters
type AnyParameters struct {
	any jsoniter.Any
}

// Get implements the govaluate.Parameters interface.
func (p AnyParameters) Get(name string) (interface{}, error) {
	any := p.any.Get(name)
	if err := any.LastError(); err != nil {
		return nil, err
	}
	switch any.ValueType() {
	case jsoniter.InvalidValue:
		return nil, fmt.Errorf("dynamic: %s", any.LastError())
	case jsoniter.ObjectValue:
		return AnyParameters{any: any}, any.LastError()
	default:
		return any.GetInterface(), any.LastError()
	}

}

// getFields gets a map of struct fields by name from a reflect.Value
func getFields(v reflect.Value) map[string]structField {
	typ := v.Type()
	numField := v.NumField()
	result := make(map[string]structField, numField)
	for i := 0; i < numField; i++ {
		field := typ.Field(i)
		if len(field.PkgPath) != 0 {
			// unexported
			continue
		}
		value := v.Field(i)
		sf := structField{Field: field, Value: value}
		sf.JSONName, sf.OmitEmpty = sf.jsonFieldName()
		result[field.Name] = sf
	}
	return result
}

// structField is an internal convenience type
type structField struct {
	Field     reflect.StructField
	Value     reflect.Value
	JSONName  string
	OmitEmpty bool
}

func (s structField) IsEmpty() bool {
	zeroValue := reflect.Zero(s.Field.Type).Interface()
	return reflect.DeepEqual(zeroValue, s.Value.Interface())
}

func (s structField) jsonFieldName() (string, bool) {
	fieldName := s.Field.Name
	tag, ok := s.Field.Tag.Lookup("json")
	omitEmpty := false
	if ok {
		parts := strings.Split(tag, ",")
		fieldName = parts[0]
		if len(parts) > 1 && parts[1] == "omitempty" {
			omitEmpty = true
		}
	}
	return fieldName, omitEmpty
}

func getJSONFields(v reflect.Value, addressOfAttrs *byte) map[string]structField {
	typ := v.Type()
	numField := v.NumField()
	result := make(map[string]structField, numField)
	for i := 0; i < numField; i++ {
		field := typ.Field(i)
		if len(field.PkgPath) != 0 {
			// unexported
			continue
		}
		value := v.Field(i)
		sf := structField{Field: field, Value: value}
		sf.JSONName, sf.OmitEmpty = sf.jsonFieldName()
		if sf.JSONName == "-" {
			continue
		}
		if sf.OmitEmpty && sf.IsEmpty() {
			continue
		}
		if b, ok := reflect.Indirect(value).Interface().([]byte); ok {
			if len(b) > 0 && &b[0] == addressOfAttrs {
				continue
			}
		}
		// sf is a valid JSON field.
		result[sf.JSONName] = sf
	}
	return result
}

// extractExtendedAttributes selects only extended attributes from msg. It will
// ignore any fields in msg that correspond to fields in v. v must be of kind
// reflect.Struct.
func extractExtendedAttributes(v interface{}, msg []byte) ([]byte, error) {
	strukt := reflect.Indirect(reflect.ValueOf(v))
	if kind := strukt.Kind(); kind != reflect.Struct {
		return nil, fmt.Errorf("invalid type (want struct): %v", kind)
	}
	fields := getJSONFields(strukt, nil)
	stream := jsoniter.NewStream(jsoniter.ConfigDefault, nil, 4096)
	var anys map[string]jsoniter.Any
	if err := jsoniter.Unmarshal(msg, &anys); err != nil {
		return nil, err
	}
	stream.WriteObjectStart()
	j := 0
	for _, any := range sortAnys(anys) {
		_, ok := fields[any.Name]
		if ok {
			// Not a extended attribute
			continue
		}
		if j > 0 {
			stream.WriteMore()
		}
		j++
		stream.WriteObjectField(any.Name)
		any.WriteTo(stream)
	}
	stream.WriteObjectEnd()
	return stream.Buffer(), nil
}

// Unmarshal decodes msg into v, storing what fields it can into the basic
// fields of the struct, and storing the rest into its extended attributes.
func Unmarshal(msg []byte, v AttrSetter) error {
	if _, ok := v.(json.Unmarshaler); ok {
		// Can't safely call UnmarshalJSON here without potentially causing an
		// infinite recursion. Copy the struct into a new type that doesn't
		// implement the method.
		oldVal := reflect.Indirect(reflect.ValueOf(v))
		typ := oldVal.Type()
		numField := typ.NumField()
		fields := make([]reflect.StructField, 0, numField)
		for i := 0; i < numField; i++ {
			field := typ.Field(i)
			if len(field.PkgPath) == 0 {
				fields = append(fields, field)
			}
		}
		newType := reflect.StructOf(fields)
		newPtr := reflect.New(newType)
		newVal := reflect.Indirect(newPtr)
		if err := json.Unmarshal(msg, newPtr.Interface()); err != nil {
			return err
		}
		for _, field := range fields {
			oldVal.FieldByName(field.Name).Set(newVal.FieldByName(field.Name))
		}
	} else {
		if err := json.Unmarshal(msg, v); err != nil {
			return err
		}
	}

	attrs, err := extractExtendedAttributes(v, msg)
	if err != nil {
		return err
	}
	v.SetExtendedAttributes(attrs)
	return nil
}

// Marshal encodes the struct fields in v that are valid to encode.
// It also encodes any extended attributes that are defined. Marshal
// respects the encoding/json rules regarding exported fields, and tag
// semantics. If v's kind is not reflect.Struct, an error will be returned.
func Marshal(v AttrGetter) ([]byte, error) {
	s := jsoniter.NewStream(jsoniter.ConfigDefault, nil, 4096)
	s.WriteObjectStart()

	if err := encodeStructFields(v, s); err != nil {
		return nil, err
	}

	extended := v.GetExtendedAttributes()
	if len(extended) > 0 {
		if err := encodeExtendedFields(extended, s); err != nil {
			return nil, err
		}
	}

	s.WriteObjectEnd()

	return s.Buffer(), nil
}

func encodeStructFields(v AttrGetter, s *jsoniter.Stream) error {
	strukt := reflect.Indirect(reflect.ValueOf(v))
	if kind := strukt.Kind(); kind != reflect.Struct {
		return fmt.Errorf("invalid type (want struct): %v", kind)
	}
	attrs := v.GetExtendedAttributes()
	if len(attrs) == 0 {
		return nil
	}
	addressOfAttrs := &attrs[0]
	m := getJSONFields(strukt, addressOfAttrs)
	fields := make([]structField, 0, len(m))
	for _, s := range m {
		fields = append(fields, s)
	}
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].JSONName < fields[j].JSONName
	})
	for i, field := range fields {
		if i > 0 {
			s.WriteMore()
		}
		s.WriteObjectField(field.JSONName)
		s.WriteVal(field.Value.Interface())
	}
	return nil
}

type anyT struct {
	Name string
	jsoniter.Any
}

func sortAnys(m map[string]jsoniter.Any) []anyT {
	anys := make([]anyT, 0, len(m))
	for key, any := range m {
		anys = append(anys, anyT{Name: key, Any: any})
	}
	sort.Slice(anys, func(i, j int) bool {
		return anys[i].Name < anys[j].Name
	})
	return anys
}

func encodeExtendedFields(extended []byte, s *jsoniter.Stream) error {
	var anys map[string]jsoniter.Any
	if err := jsoniter.Unmarshal(extended, &anys); err != nil {
		return err
	}
	for _, any := range sortAnys(anys) {
		s.WriteMore()
		s.WriteObjectField(any.Name)
		any.WriteTo(s)
	}
	return nil
}