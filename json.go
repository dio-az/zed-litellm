package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Bedrock reads from the prompt cache only if the start of the request is the same, byte for byte.
// So the relay must keep the key order of Zed, and must not change the bytes of a value that it
// does not edit. A Go map loses the key order, so an object is a list of fields. Decoding into
// json.RawMessage keeps the exact bytes of a value.

var errSyntax = errors.New("malformed JSON")

type field struct {
	key string
	val json.RawMessage
}

type object []field

func parseObject(data []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	obj := object{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		obj = append(obj, field{key: key.(string), val: val})
	}
	return obj, expectEnd(dec, '}')
}

func parseArray(data []byte) ([]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := expectDelim(dec, '['); err != nil {
		return nil, err
	}
	elems := []json.RawMessage{}
	for dec.More() {
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		elems = append(elems, val)
	}
	return elems, expectEnd(dec, ']')
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if t != want {
		return errSyntax
	}
	return nil
}

func expectEnd(dec *json.Decoder, want json.Delim) error {
	if err := expectDelim(dec, want); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errSyntax
	}
	return nil
}

func (o object) get(key string) (json.RawMessage, bool) {
	for _, f := range o {
		if f.key == key {
			return f.val, true
		}
	}
	return nil, false
}

func (o *object) set(key string, val json.RawMessage) {
	for i := range *o {
		if (*o)[i].key == key {
			(*o)[i].val = val
			return
		}
	}
	*o = append(*o, field{key: key, val: val})
}

func (o *object) del(key string) bool {
	for i, f := range *o {
		if f.key == key {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return true
		}
	}
	return false
}

func (o object) bytes() json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(jsonString(f.key))
		buf.WriteByte(':')
		buf.Write(f.val)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func arrayBytes(elems []json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, e := range elems {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(e)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

func jsonString(s string) string {
	b, _ := json.Marshal(s) // a string always encodes
	return string(b)
}

func isString(raw json.RawMessage) bool {
	return len(raw) > 0 && raw[0] == '"'
}

// The arguments match the results of get, so that stringValue(obj.get("key")) works.
func stringValue(raw json.RawMessage, ok bool) string {
	var s string
	if !ok || !isString(raw) || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
