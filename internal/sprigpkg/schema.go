package sprigpkg

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Gallery files are checked against JSON Schemas (schemas/*.json, also
// published in docs/gallery.md) before anything reads them as a sprig.
// The validator here implements the part of JSON Schema those files use:
// type, properties, required, additionalProperties (false or a schema),
// items, enum, pattern, minLength, maxLength, maxItems, maxProperties,
// minimum and maximum. A schema using anything else fails to load, so a
// schema can't quietly ask for a check that isn't made.

//go:embed schemas/*.json
var schemaFS embed.FS

type schema struct {
	Type                 string             `json:"type"`
	Properties           map[string]*schema `json:"properties"`
	Required             []string           `json:"required"`
	AdditionalProperties json.RawMessage    `json:"additionalProperties"`
	Items                *schema            `json:"items"`
	Enum                 []any              `json:"enum"`
	Pattern              string             `json:"pattern"`
	MinLength            *int               `json:"minLength"`
	MaxLength            *int               `json:"maxLength"`
	MaxItems             *int               `json:"maxItems"`
	MaxProperties        *int               `json:"maxProperties"`
	Minimum              *float64           `json:"minimum"`
	Maximum              *float64           `json:"maximum"`

	re         *regexp.Regexp
	addl       *schema // additionalProperties as a schema
	addlClosed bool    // additionalProperties: false
}

// Keywords a schema may use; anything else is refused when it loads.
var schemaKeywords = map[string]bool{
	"$schema": true, "$id": true, "title": true, "description": true,
	"type": true, "properties": true, "required": true, "additionalProperties": true, "items": true,
	"enum": true, "pattern": true, "minLength": true, "maxLength": true, "maxItems": true,
	"maxProperties": true, "minimum": true, "maximum": true,
}

func compileSchema(raw json.RawMessage, where string) (*schema, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	for k := range keys {
		if !schemaKeywords[k] {
			return nil, fmt.Errorf("%s: unsupported schema keyword %q", where, k)
		}
	}
	var s schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	if s.Pattern != "" {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		s.re = re
	}
	// Nested schemas are compiled from their raw form, so their keywords
	// are checked and their patterns compiled too.
	if raw, ok := keys["properties"]; ok {
		var props map[string]json.RawMessage
		if err := json.Unmarshal(raw, &props); err != nil {
			return nil, fmt.Errorf("%s.properties: %w", where, err)
		}
		s.Properties = map[string]*schema{}
		for name, pr := range props {
			sub, err := compileSchema(pr, where+"."+name)
			if err != nil {
				return nil, err
			}
			s.Properties[name] = sub
		}
	}
	if raw, ok := keys["items"]; ok {
		sub, err := compileSchema(raw, where+"[]")
		if err != nil {
			return nil, err
		}
		s.Items = sub
	}
	if len(s.AdditionalProperties) > 0 {
		switch strings.TrimSpace(string(s.AdditionalProperties)) {
		case "false":
			s.addlClosed = true
		case "true":
		default:
			sub, err := compileSchema(s.AdditionalProperties, where+".*")
			if err != nil {
				return nil, err
			}
			s.addl = sub
		}
	}
	return &s, nil
}

var (
	schemasOnce sync.Once
	schemas     map[string]*schema
	schemasErr  error
)

// loadSchema returns a compiled schema by name (without .schema.json).
func loadSchema(name string) *schema {
	schemasOnce.Do(func() {
		schemas = map[string]*schema{}
		entries, err := schemaFS.ReadDir("schemas")
		if err != nil {
			schemasErr = err
			return
		}
		for _, e := range entries {
			b, err := schemaFS.ReadFile("schemas/" + e.Name())
			if err != nil {
				schemasErr = err
				return
			}
			s, err := compileSchema(b, e.Name())
			if err != nil {
				schemasErr = err
				return
			}
			schemas[strings.TrimSuffix(e.Name(), ".schema.json")] = s
		}
	})
	if schemasErr != nil {
		panic("sprigpkg: bad embedded schema: " + schemasErr.Error())
	}
	s, ok := schemas[name]
	if !ok {
		panic("sprigpkg: no schema " + name)
	}
	return s
}

// SchemaJSON returns an embedded schema's text, for docs and tools.
func SchemaJSON(name string) ([]byte, error) {
	return schemaFS.ReadFile("schemas/" + name + ".schema.json")
}

// validate checks a decoded JSON or YAML value against s. path names the
// value in errors.
func (s *schema) validate(v any, path string) error {
	if s.Type != "" && !hasType(v, s.Type) {
		return fmt.Errorf("%s must be %s", path, article(s.Type))
	}
	if len(s.Enum) > 0 {
		ok := false
		for _, e := range s.Enum {
			if fmt.Sprint(e) == fmt.Sprint(v) && hasType(v, typeOf(e)) {
				ok = true
				break
			}
		}
		if !ok {
			var opts []string
			for _, e := range s.Enum {
				opts = append(opts, fmt.Sprint(e))
			}
			return fmt.Errorf("%s must be one of %s", path, strings.Join(opts, ", "))
		}
	}
	switch x := v.(type) {
	case string:
		n := utf8.RuneCountInString(x)
		if s.MinLength != nil && n < *s.MinLength {
			return fmt.Errorf("%s is too short (at least %d characters)", path, *s.MinLength)
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			return fmt.Errorf("%s is too long (at most %d characters)", path, *s.MaxLength)
		}
		if s.re != nil && !s.re.MatchString(x) {
			return fmt.Errorf("%s %q doesn't match %s", path, clip(x), s.Pattern)
		}
	case []any:
		if s.MaxItems != nil && len(x) > *s.MaxItems {
			return fmt.Errorf("%s has too many items (at most %d)", path, *s.MaxItems)
		}
		if s.Items != nil {
			for i, it := range x {
				if err := s.Items.validate(it, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		if s.MaxProperties != nil && len(x) > *s.MaxProperties {
			return fmt.Errorf("%s has too many entries (at most %d)", path, *s.MaxProperties)
		}
		for _, r := range s.Required {
			if _, ok := x[r]; !ok {
				return fmt.Errorf("%s needs %s", path, r)
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub := s.Properties[k]
			switch {
			case sub != nil:
			case s.addlClosed:
				return fmt.Errorf("%s: %q isn't allowed here", path, clip(k))
			case s.addl != nil:
				sub = s.addl
			default:
				continue
			}
			if err := sub.validate(x[k], path+"."+k); err != nil {
				return err
			}
		}
	default:
		if f, ok := number(v); ok {
			if s.Minimum != nil && f < *s.Minimum {
				return fmt.Errorf("%s must be at least %v", path, *s.Minimum)
			}
			if s.Maximum != nil && f > *s.Maximum {
				return fmt.Errorf("%s must be at most %v", path, *s.Maximum)
			}
		}
	}
	return nil
}

func clip(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

func article(t string) string {
	switch t {
	case "object", "array", "integer":
		return "an " + t
	}
	return "a " + t
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func typeOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	}
	if f, ok := number(v); ok {
		if f == math.Trunc(f) {
			return "integer"
		}
		return "number"
	}
	return "unknown"
}

func hasType(v any, t string) bool {
	got := typeOf(v)
	return got == t || (t == "number" && got == "integer")
}

// normalize turns a YAML-decoded value into JSON's shapes: maps with
// string keys only. A non-string key is an error.
func normalize(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			x[k] = n
		}
		return x, nil
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("keys must be text (found %v)", k)
			}
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			out[ks] = n
		}
		return out, nil
	case []any:
		for i, e := range x {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			x[i] = n
		}
		return x, nil
	}
	return v, nil
}
