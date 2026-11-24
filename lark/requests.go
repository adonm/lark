package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"reflect"
	"strings"
)

// readJSONBody enforces the framing every REST endpoint shares, then returns
// the raw bytes for a typed or object decode.
func readJSONBody(r *http.Request) ([]byte, error) {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != jsonType {
		return nil, problem(415, "Expected application/json")
	}
	lengths := r.Header.Values("Content-Length")
	if len(r.TransferEncoding) != 0 || len(lengths) > 1 {
		return nil, problem(400, "Request requires a single Content-Length")
	}
	if len(lengths) == 0 && r.ContentLength <= 0 {
		return nil, problem(411, "Content-Length required")
	}
	if len(lengths) != 0 && len(lengths[0]) > 12 {
		return nil, problem(400, "Invalid Content-Length")
	}
	if r.ContentLength > maxJSON {
		return nil, problem(413, "Request body too large")
	}
	if r.ContentLength < 0 {
		return nil, problem(400, "Invalid Content-Length")
	}
	buf := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		var timed net.Error
		if errors.As(err, &timed) && timed.Timeout() {
			return nil, problem(408, "Request body timed out")
		}
		return nil, problem(400, "Incomplete request body")
	}
	return buf, nil
}

// jsonBody decodes a request body into the open object used by tools that
// accept a published schema; typed endpoints use decodeRequest instead.
func jsonBody(r *http.Request) (object, error) {
	buf, err := readJSONBody(r)
	if err != nil {
		return nil, err
	}
	return decodeObject(buf)
}

// decodeRequest decodes a REST body into a typed request and rejects unknown
// fields, so a misspelled key fails loudly instead of being ignored.
func decodeRequest(r *http.Request, dst any) error {
	buf, err := readJSONBody(r)
	if err != nil {
		return err
	}
	return decodeInto(buf, dst)
}

// decodeInput re-decodes MCP arguments that already passed the published
// schema into their typed request.
func decodeInput(src object, dst any) error {
	raw, err := json.Marshal(src)
	if err != nil {
		return problem(500, "Cannot encode request")
	}
	return decodeInto(raw, dst)
}

func decodeInto(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if dec.More() {
		return problem(400, "Expected a single JSON value")
	}
	return nil
}

func decodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return fieldError(typeErr.Field, "expected %s", jsonKind(typeErr.Type))
	}
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return fieldError(strings.Trim(field, `"`), "unknown field")
	}
	return problem(400, "Invalid request: "+err.Error())
}

func jsonKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "an array"
	case reflect.Map, reflect.Struct:
		return "an object"
	}
	return t.String()
}

// terminalPatch is one terminal change from the patch tool or a REST body.
type terminalPatch struct {
	TerminalID string `json:"terminal_id,omitempty"`
	Volume     *int   `json:"volume,omitempty"`
	Sleep      *bool  `json:"sleep,omitempty"`
}

type volumeRequest struct {
	Volume *int `json:"volume"`
}

type remoteSayRequest struct {
	Text string `json:"text"`
}

type remoteListenRequest struct {
	Listening *bool `json:"listening"`
}

type remoteVolumeRequest struct {
	Terminal string `json:"terminal"`
	Volume   *int   `json:"volume"`
}

type remoteVoiceRequest struct {
	Voice string `json:"voice"`
}

type remoteStoryRequest struct {
	Story string `json:"story"`
}

type archiveHistoryRequest struct {
	ArchiveHistory *bool `json:"archive_history"`
}

// companionPatch is one companion update from REST, MCP, or an internal call.
// Nil fields were absent and stay unchanged.
type companionPatch struct {
	Op          *string `json:"op,omitempty"`
	Name        *string `json:"name,omitempty"`
	Personality *string `json:"personality,omitempty"`
	Form        *string `json:"form,omitempty"`
	Voice       *string `json:"voice,omitempty"`
	Listening   *bool   `json:"listening,omitempty"`
	Configured  *bool   `json:"configured,omitempty"`
	Portrait    object  `json:"portrait,omitempty"`
}

// object renders the patch for the companion store, which still applies its
// own field validation.
func (p companionPatch) object() object {
	out := object{}
	if p.Op != nil {
		out["op"] = *p.Op
	}
	for key, value := range map[string]any{
		"name": p.Name, "personality": p.Personality, "form": p.Form, "voice": p.Voice,
		"listening": p.Listening, "configured": p.Configured,
	} {
		switch v := value.(type) {
		case *string:
			if v != nil {
				out[key] = *v
			}
		case *bool:
			if v != nil {
				out[key] = *v
			}
		}
	}
	if p.Portrait != nil {
		out["portrait"] = p.Portrait
	}
	return out
}

// captureRequest is the one capture body shared by `lark capture` (through
// /api/capture) and the scoped remote page.
type captureRequest struct {
	CompanionID string `json:"companion_id,omitempty"`
	Seconds     *int   `json:"seconds,omitempty"`
}
