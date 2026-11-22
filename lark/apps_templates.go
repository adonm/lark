package main

import (
	_ "embed"
	"encoding/json"
	"maps"
	"regexp"
	"slices"
)

//go:embed apps_templates.js
var appTemplatesJS string

type appTemplate struct {
	title  string
	fields object
}

func closedObject(props object, required ...string) object {
	if required == nil {
		required = []string{}
	}
	return object{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

var templateCatalog = func() map[string]appTemplate {
	text := object{"type": "string"}
	num := object{"type": "integer"}
	array := func(item object) object { return object{"type": "array", "items": item} }
	shapes := append(slices.Sorted(maps.Keys(pxAppSprites)), "circle", "diamond", "square", "triangle")
	shape := object{"type": "string", "enum": shapes}
	return map[string]appTemplate{
		"quiz":                {"Quiz", object{"questions": array(closedObject(object{"prompt": text, "choices": array(text), "answer": num}, "prompt", "choices", "answer"))}},
		"spot-the-difference": {"Spot the Difference", object{"tiles": array(closedObject(object{"left": shape, "right": shape}, "left", "right"))}},
		"drawing":             {"Drawing", object{"colors": array(text)}},
		"pet":                 {"My Pet", object{"name": text, "pet_color": text}},
		"timer":               {"Countdown", object{"seconds": num}},
		"picture-math":        {"Picture Math", object{"problems": array(closedObject(object{"left": num, "right": num}, "left", "right"))}},
		"memory-match":        {"Memory Match", object{"cards": array(text)}},
		"fireworks":           {"Fireworks", object{"colors": array(text)}},
	}
}()

var templateCommon = object{"title": object{"type": "string"}, "theme": object{"type": "string", "enum": []string{"warm", "night", "mint", "sky"}}}

func appTemplateSchema(name string) object {
	fields := maps.Clone(templateCommon)
	if name != "" {
		maps.Copy(fields, templateCatalog[name].fields)
	} else {
		for _, t := range templateCatalog {
			maps.Copy(fields, t.fields)
		}
	}
	return closedObject(fields)
}

func templateNames() []string { return slices.Sorted(maps.Keys(templateCatalog)) }

func propertySchema(schema object, key string) (object, bool) {
	if props, ok := schema["properties"].(object); ok {
		if child, ok := props[key].(object); ok {
			return child, true
		}
	}
	if patterns, ok := schema["patternProperties"].(object); ok {
		for pattern, child := range patterns {
			if match, _ := regexp.MatchString(pattern, key); match {
				return child.(object), true
			}
		}
	}
	return nil, false
}

func validateTemplateValue(v any, schema object, path string) error {
	if branches, ok := schema["anyOf"].([]object); ok {
		for _, branch := range branches {
			if validateTemplateValue(v, branch, path) == nil {
				return nil
			}
		}
		return fieldError(path, "invalid value")
	}
	switch schema["type"] {
	case "object":
		if o, ok := v.(object); ok {
			return validateTemplateObject(o, schema, path)
		}
		return fieldError(path, "expected an object")
	case "array":
		list, ok := v.([]any)
		if !ok {
			return fieldError(path, "expected an array")
		}
		item := schema["items"].(object)
		for i, entry := range list {
			if err := validateTemplateValue(entry, item, path); err != nil {
				return err
			}
			_ = i
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			return fieldError(path, "expected a string")
		}
		if choices, ok := schema["enum"].([]string); ok && !slices.Contains(choices, s) {
			return fieldError(path, "unsupported value %q", s)
		}
	case "integer":
		switch n := v.(type) {
		case int:
		case float64:
			if n != float64(int(n)) {
				return fieldError(path, "expected an integer")
			}
		default:
			return fieldError(path, "expected an integer")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fieldError(path, "expected a boolean")
		}
	}
	return nil
}

func validateTemplateObject(o object, schema object, path string) error {
	for _, req := range schema["required"].([]string) {
		if _, ok := o[req]; !ok {
			return fieldError(path+"."+req, "missing required field")
		}
	}
	for key, val := range o {
		child, ok := propertySchema(schema, key)
		if !ok {
			return fieldError(path+"."+key, "unsupported field")
		}
		if err := validateTemplateValue(val, child, path+"."+key); err != nil {
			return err
		}
	}
	return nil
}

func (a *savedApp) setTemplate(name string, data any) error {
	if _, ok := templateCatalog[name]; !ok {
		return fieldError("template", "unknown template %q", name)
	}
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return fieldError("data", "expected a JSON object")
		}
		raw = b
	}
	if err := validateTemplateData(name, raw); err != nil {
		return err
	}
	a.Template = name
	a.Data = raw
	a.Code = ""
	if a.Title == "" {
		a.Title = templateCatalog[name].title
	}
	if name == "timer" || name == "fireworks" {
		a.TickMS = 1000
		a.MaxTicks = 240
	}
	return nil
}

func (a *appInstance) installTemplate() (string, error) {
	var data any = map[string]any{}
	if len(a.spec.Data) > 0 {
		if err := json.Unmarshal(a.spec.Data, &data); err != nil {
			return "", err
		}
	}
	_ = a.vm.Set("templateName", a.spec.Template)
	_ = a.vm.Set("templateTitle", a.spec.Title)
	_ = a.vm.Set("templateData", data)
	return appTemplatesJS, nil
}

func validateTemplateData(name string, raw json.RawMessage) error {
	if _, ok := templateCatalog[name]; !ok {
		return fieldError("template", "unknown template %q", name)
	}
	if len(raw) == 0 {
		return nil
	}
	var data object
	if err := json.Unmarshal(raw, &data); err != nil {
		return fieldError("data", "expected a JSON object")
	}
	return validateTemplateObject(data, appTemplateSchema(name), "data")
}
