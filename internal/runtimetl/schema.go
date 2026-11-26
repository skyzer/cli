// Package runtimetl interprets the published Telegram Desktop API schema.
package runtimetl

import (
	"bufio"
	"bytes"
	"fmt"
	"math/bits"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
	flag string
	bit  uint32
}

type Definition struct {
	Name   string  `json:"name"`
	ID     uint32  `json:"id"`
	Fields []Field `json:"parameters"`
	Result string  `json:"result"`
}

type Schema struct {
	Layer        int
	methods      map[string]Definition
	constructors map[string]Definition
	ids          map[uint32]Definition
}

var (
	layerLine = regexp.MustCompile(`^// LAYER ([0-9]+)$`)
	tlName    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)?$`)
)

func Parse(input []byte) (*Schema, error) {
	s := &Schema{
		methods: map[string]Definition{}, constructors: map[string]Definition{},
		ids: map[uint32]Definition{},
	}
	methods := false
	lineNo := 0
	scan := bufio.NewScanner(bytes.NewReader(input))
	for scan.Scan() {
		lineNo++
		line := strings.TrimSpace(scan.Text())
		if match := layerLine.FindStringSubmatch(line); match != nil {
			layer, err := strconv.Atoi(match[1])
			if err != nil || layer <= 0 || s.Layer != 0 {
				return nil, fmt.Errorf("invalid layer at line %d", lineNo)
			}
			s.Layer = layer
		}
		line, _, _ = strings.Cut(line, "//")
		line = strings.TrimSpace(line)
		switch line {
		case "", "vector#1cb5c415 {t:Type} # [ t ] = Vector t;":
			continue
		case "---functions---":
			methods = true
			continue
		case "---types---":
			methods = false
			continue
		}
		entry, err := parseDefinition(line)
		if err != nil {
			return nil, fmt.Errorf("schema line %d: %w", lineNo, err)
		}
		if _, exists := s.ids[entry.ID]; exists {
			return nil, fmt.Errorf("duplicate TL ID at line %d", lineNo)
		}
		entries := s.constructors
		if methods {
			entries = s.methods
		}
		if _, exists := entries[entry.Name]; exists {
			return nil, fmt.Errorf("duplicate TL name at line %d", lineNo)
		}
		entries[entry.Name] = entry
		s.ids[entry.ID] = entry
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	if s.Layer == 0 || len(s.methods) == 0 || len(s.constructors) == 0 {
		return nil, fmt.Errorf("incomplete Telegram API schema")
	}
	return s, nil
}

func parseDefinition(line string) (Definition, error) {
	left, right, ok := strings.Cut(line, "=")
	head := strings.Fields(left)
	result, end := strings.CutSuffix(strings.TrimSpace(right), ";")
	if !ok || !end || len(head) == 0 || !validType(strings.TrimSpace(result)) {
		return Definition{}, fmt.Errorf("invalid TL definition")
	}
	name, hexID, _ := strings.Cut(head[0], "#")
	id, err := strconv.ParseUint(hexID, 16, 32)
	if !tlName.MatchString(name) || err != nil {
		return Definition{}, fmt.Errorf("invalid TL name or ID")
	}
	entry := Definition{Name: name, ID: uint32(id), Result: strings.TrimSpace(result), Fields: []Field{}}
	seen := make(map[string]string, len(head)-1)
	for _, token := range head[1:] {
		if token == "{X:Type}" {
			continue
		}
		key, typ, _ := strings.Cut(token, ":")
		if _, duplicate := seen[key]; duplicate || !tlName.MatchString(key) || typ == "" {
			return Definition{}, fmt.Errorf("invalid TL parameter")
		}
		field := Field{Name: key, Type: typ}
		if mask, optional, found := strings.Cut(typ, "?"); found {
			flag, bit, _ := strings.Cut(mask, ".")
			value, err := strconv.ParseUint(bit, 10, 5)
			if err != nil || seen[flag] != "#" {
				return Definition{}, fmt.Errorf("invalid TL flag reference")
			}
			field.flag, field.bit, field.Type = flag, uint32(1)<<value, optional
		}
		if !validType(field.Type) || (field.flag == "" && field.Type == "true") {
			return Definition{}, fmt.Errorf("invalid type for %s", field.Name)
		}
		seen[key] = field.Type
		entry.Fields = append(entry.Fields, field)
	}
	return entry, nil
}

func validType(value string) bool {
	for strings.HasPrefix(value, "Vector<") && strings.HasSuffix(value, ">") {
		value = value[7 : len(value)-1]
	}
	return value == "#" || value == "!X" || tlName.MatchString(value)
}

func (s *Schema) Names() []string {
	names := make([]string, 0, len(s.methods))
	for name := range s.methods {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (s *Schema) Describe(name string) (Definition, error) {
	entry, ok := s.methods[name]
	if !ok {
		entry, ok = s.constructors[name]
	}
	if !ok {
		return Definition{}, fmt.Errorf("unknown Telegram schema name %q", name)
	}
	entry.Fields = slices.Clone(entry.Fields)
	for i := range entry.Fields {
		field := &entry.Fields[i]
		if field.flag != "" {
			field.Type = fmt.Sprintf("%s.%d?%s", field.flag, bits.TrailingZeros32(field.bit), field.Type)
		}
	}
	return entry, nil
}
