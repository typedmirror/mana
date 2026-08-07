package object

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ParseJSON decodes JSON into Mana values, preserving object key order.
//
// encoding/json's map[string]any would lose that order, and order is visible
// here: records render in insertion order so a report reads the same way twice.
func ParseJSON(src string) (Value, error) {
	dec := json.NewDecoder(strings.NewReader(src))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	// Trailing content means the input was not one JSON document, which usually
	// means it was not JSON at all.
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing content after JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return decodeFrom(dec, tok)
}

func decodeFrom(dec *json.Decoder, tok json.Token) (Value, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return decodeRecord(dec)
		case '[':
			return decodeList(dec)
		}
		return nil, fmt.Errorf("unexpected %q in JSON", t)
	case string:
		return String(t), nil
	case bool:
		return Bool(t), nil
	case nil:
		return Null{}, nil
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return nil, err
		}
		return Number(f), nil
	}
	return nil, fmt.Errorf("unsupported JSON value %T", tok)
}

func decodeRecord(dec *json.Decoder) (Value, error) {
	rec := NewRecord()
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			return rec, nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("expected a JSON object key, got %v", tok)
		}
		val, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		rec.Set(key, val)
	}
}

func decodeList(dec *json.Decoder) (Value, error) {
	list := &List{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if d, ok := tok.(json.Delim); ok && d == ']' {
			return list, nil
		}
		val, err := decodeFrom(dec, tok)
		if err != nil {
			return nil, err
		}
		list.Elements = append(list.Elements, val)
	}
}
