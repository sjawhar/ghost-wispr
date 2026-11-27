package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

// attendeeSigningForm re-encodes a webhook body the way Attendee's
// sign_payload encodes the object it signs (Attendee v1.100.6
// bots/webhook_utils.py:68-79):
//
//	json.dumps(payload, sort_keys=True, ensure_ascii=False, separators=(",", ":"))
//
// Attendee posts a different text of the same object: requests' json=
// argument writes json.dumps(payload, allow_nan=False), with keys in their
// insertion order, ", " and ": " separators, and every non-ASCII character as
// a \u escape (bots/tasks/deliver_webhook_task.py:144-155). So the signature
// holds only over the re-encoded body.
//
// Python's form differs from Go's encoding/json: keys sort by code point;
// strings escape only '"', '\\' and the characters below U+0020, so '<', '>',
// '&', U+2028, U+2029, U+007F and every non-ASCII character are written as
// themselves; and a number is written as Python's repr of its value. Both of
// Attendee's json.dumps calls print the same Python object with float.__repr__
// and int.__repr__, so the body already holds each number exactly as the
// signed text does: the encoder copies each number's text unchanged, which
// keeps 1.0 apart from 1, 1e-05 and 1e+16 as Python writes them, and an
// integer of any size whole.
func attendeeSigningForm(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the JSON value")
	}
	var out bytes.Buffer
	out.Grow(len(body))
	if err := appendPythonJSON(&out, v); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// appendPythonJSON writes v, as encoding/json decodes it with UseNumber, in
// Python's sorted, compact, non-ASCII form.
func appendPythonJSON(out *bytes.Buffer, v any) error {
	switch v := v.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case json.Number:
		out.WriteString(string(v))
	case string:
		appendPythonString(out, v)
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendPythonJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		// Go orders strings by their UTF-8 bytes, which is code point
		// order, the order of Python's sort_keys.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			appendPythonString(out, k)
			out.WriteByte(':')
			if err := appendPythonJSON(out, v[k]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unexpected JSON value %T", v)
	}
	return nil
}

const lowerHex = "0123456789abcdef"

// appendPythonString writes s as Python's json module does with
// ensure_ascii=False (CPython Modules/_json.c, escape_unicode): '"' and '\\'
// with a backslash, \b \f \n \r \t by name, the other characters below U+0020
// as \u00xx in lower-case hex, and every other character as itself.
func appendPythonString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	start := 0
	for i := range len(s) {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		out.WriteString(s[start:i])
		out.WriteByte('\\')
		switch c {
		case '"', '\\':
			out.WriteByte(c)
		case '\b':
			out.WriteByte('b')
		case '\f':
			out.WriteByte('f')
		case '\n':
			out.WriteByte('n')
		case '\r':
			out.WriteByte('r')
		case '\t':
			out.WriteByte('t')
		default:
			out.WriteString("u00")
			out.WriteByte(lowerHex[c>>4])
			out.WriteByte(lowerHex[c&0xf])
		}
		start = i + 1
	}
	out.WriteString(s[start:])
	out.WriteByte('"')
}
