package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

const maxSpriteSide = 64

var pixelColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func uiArray(item object, lo, hi int) object {
	return object{"type": "array", "items": item, "minItems": lo, "maxItems": hi}
}

func uiString(maxLen int) object { return object{"type": "string", "maxLength": maxLen} }

func uiInt(lo, hi int) object {
	return object{"type": "integer", "minimum": lo, "maximum": hi}
}

var (
	pixelHexSchema     = object{"type": "string", "pattern": pixelColor.String()}
	pixelRowSchema     = object{"type": "string", "pattern": `^[!-~]{2,64}$`}
	pixelRowsSchema    = uiArray(pixelRowSchema, 2, maxSpriteSide)
	pixelPaletteSchema = object{"type": "object", "additionalProperties": false,
		"patternProperties": object{`^[!-\x2D/-~]$`: pixelHexSchema}}
)

func readUIValue[T any](raw any, schema object, path string) (T, error) {
	var v T
	if err := validateTemplateValue(raw, schema, path); err != nil {
		return v, err
	}
	// Schema validation rejects unknown/null fields and checks dynamic map keys
	// before decoding into the same typed inputs from MCP and JavaScript.
	data, err := json.Marshal(raw)
	if err == nil {
		err = json.Unmarshal(data, &v)
	}
	return v, err
}

func decodeUIJSON(s string) (any, error) {
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v any
	err := d.Decode(&v)
	return v, err
}
