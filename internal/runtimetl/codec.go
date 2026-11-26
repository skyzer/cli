package runtimetl

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/bin"
)

const (
	maxDepth    = 64
	maxVector   = 16_384
	maxResponse = 1 << 20
)

// Call implements gotd's request encoder and response decoder for one TL method.
type Call struct {
	schema   *Schema
	id       uint32
	result   string
	request  []byte
	Response any
}

func (s *Schema) Prepare(name string, arguments json.RawMessage) (*Call, error) {
	method, ok := s.methods[name]
	if !ok {
		return nil, fmt.Errorf("unknown Telegram method %q", name)
	}
	var out bin.Buffer
	result, err := s.writeObject(&out, method, arguments, 0)
	if err != nil {
		return nil, err
	}
	return &Call{schema: s, id: method.ID, request: out.Buf, result: result}, nil
}

func (c *Call) TypeID() uint32 { return c.id }

func (c *Call) Encode(out *bin.Buffer) error {
	out.Put(c.request)
	return nil
}

func (c *Call) Decode(input *bin.Buffer) error {
	if input.Len() > maxResponse {
		return fmt.Errorf("Telegram response exceeds 1 MiB")
	}
	decoded, err := c.schema.readValue(input, c.result, 0)
	if err != nil {
		return err
	}
	if input.Len() != 0 {
		return fmt.Errorf("Telegram response has trailing TL data")
	}
	c.Response = decoded
	return nil
}

func (s *Schema) writeObject(out *bin.Buffer, def Definition, arguments json.RawMessage, depth int) (string, error) {
	var args map[string]json.RawMessage
	if depth > maxDepth || json.Unmarshal(arguments, &args) != nil || args == nil {
		return "", fmt.Errorf("expected a TL argument object")
	}
	flags := make(map[string]uint32)
	for _, field := range def.Fields {
		if _, present := args[field.Name]; present && field.flag != "" {
			flags[field.flag] |= field.bit
		}
	}
	out.PutID(def.ID)
	result := def.Result
	for _, field := range def.Fields {
		if field.Type == "#" {
			out.PutUint32(flags[field.Name])
			continue
		}
		if field.flag != "" && flags[field.flag]&field.bit == 0 {
			continue
		}
		raw, present := args[field.Name]
		if !present {
			return "", fmt.Errorf("missing Telegram argument %q", field.Name)
		}
		delete(args, field.Name)
		nested, err := s.writeValue(out, field.Type, raw, depth+1)
		if err != nil {
			return "", fmt.Errorf("%s: %w", field.Name, err)
		}
		if field.Type == "!X" && result == "X" {
			result = nested
		}
	}
	if len(args) != 0 {
		return "", fmt.Errorf("unrecognized Telegram arguments")
	}
	if result == "X" {
		return "", fmt.Errorf("could not infer the nested RPC result type")
	}
	return result, nil
}

func (s *Schema) writeValue(out *bin.Buffer, typ string, raw json.RawMessage, depth int) (string, error) {
	if depth > maxDepth || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("unexpected null or nesting limit")
	}
	if item, vector := vectorItem(typ); vector {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil || len(list) > maxVector {
			return "", fmt.Errorf("expected a TL vector")
		}
		out.PutVectorHeader(len(list))
		for _, value := range list {
			if _, err := s.writeValue(out, item, value, depth+1); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	switch typ {
	case "int":
		var value int32
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("expected a 32-bit integer")
		}
		out.PutInt32(value)
	case "long":
		var decimal string
		if json.Unmarshal(raw, &decimal) != nil {
			decimal = string(raw)
		}
		value, err := strconv.ParseInt(decimal, 10, 64)
		if err != nil {
			return "", fmt.Errorf("expected a signed 64-bit decimal")
		}
		out.PutLong(value)
	case "double":
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("expected a double")
		}
		out.PutDouble(value)
	case "string":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("expected a string")
		}
		out.PutString(value)
	case "bytes", "int128", "int256":
		var encoded string
		if json.Unmarshal(raw, &encoded) != nil {
			return "", fmt.Errorf("expected a base64 string")
		}
		value, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || (typ == "int128" && len(value) != 16) || (typ == "int256" && len(value) != 32) {
			return "", fmt.Errorf("invalid TL bytes")
		}
		if typ == "bytes" {
			out.PutBytes(value)
		} else {
			out.Put(value)
		}
	case "Bool", "true":
		var value bool
		if json.Unmarshal(raw, &value) != nil || (typ == "true" && !value) {
			return "", fmt.Errorf("expected a TL boolean (omit a false presence flag)")
		}
		if typ == "Bool" {
			out.PutBool(value)
		}
	default:
		return s.writeNamedObject(out, typ, raw, depth)
	}
	return "", nil
}

func (s *Schema) writeNamedObject(out *bin.Buffer, typ string, raw json.RawMessage, depth int) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return "", fmt.Errorf("expected a TL object")
	}
	var name string
	if json.Unmarshal(fields["_"], &name) != nil || name == "" {
		return "", fmt.Errorf("expected a TL name in the _ field")
	}
	delete(fields, "_")
	def, ok := s.constructors[name]
	if typ == "!X" {
		def, ok = s.methods[name]
	}
	if !ok || (typ != "!X" && def.Result != typ) {
		return "", fmt.Errorf("incompatible TL name %q for %s", name, typ)
	}
	args, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	return s.writeObject(out, def, args, depth+1)
}

func vectorItem(typ string) (string, bool) {
	if strings.HasPrefix(typ, "Vector<") && strings.HasSuffix(typ, ">") {
		return typ[7 : len(typ)-1], true
	}
	return "", false
}

func (s *Schema) readValue(input *bin.Buffer, typ string, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("Telegram response exceeds nesting limit")
	}
	if item, vector := vectorItem(typ); vector {
		n, err := input.VectorHeader()
		if err != nil || n < 0 || n > maxVector || n > input.Len()/4 {
			return nil, fmt.Errorf("invalid TL vector response")
		}
		values := make([]any, n)
		for i := range values {
			values[i], err = s.readValue(input, item, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return values, nil
	}
	switch typ {
	case "int":
		return input.Int32()
	case "long":
		value, err := input.Long()
		return strconv.FormatInt(value, 10), err
	case "double":
		value, err := input.Double()
		if err == nil && (math.IsNaN(value) || math.IsInf(value, 0)) {
			return nil, fmt.Errorf("nonfinite TL double")
		}
		return value, err
	case "string":
		value, err := input.String()
		if err == nil && !utf8.ValidString(value) {
			return nil, fmt.Errorf("invalid TL UTF-8")
		}
		return value, err
	case "bytes":
		return input.Bytes()
	case "int128":
		data, err := input.Int128()
		return data[:], err
	case "int256":
		data, err := input.Int256()
		return data[:], err
	case "Bool":
		return input.Bool()
	case "true":
		return true, nil
	default:
		return s.readObject(input, typ, depth)
	}
}

func (s *Schema) readObject(input *bin.Buffer, typ string, depth int) (any, error) {
	id, err := input.ID()
	if err != nil {
		return nil, err
	}
	def, ok := s.ids[id]
	if !ok || def.Result != typ || s.constructors[def.Name].ID != id {
		return nil, fmt.Errorf("unexpected TL constructor 0x%08x for %s", id, typ)
	}
	fields := map[string]any{"_": def.Name}
	flags := make(map[string]uint32)
	for _, field := range def.Fields {
		if field.Type == "#" {
			flags[field.Name], err = input.Uint32()
		} else if field.flag == "" || flags[field.flag]&field.bit != 0 {
			fields[field.Name], err = s.readValue(input, field.Type, depth+1)
		}
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", def.Name, field.Name, err)
		}
	}
	return fields, nil
}
