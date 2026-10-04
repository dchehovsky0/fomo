package fomo

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// The API is unofficial and undocumented, so scalar fields are decoded
// leniently: numbers may arrive as strings, ids as numbers, anything as null.
// A single odd field must not make the whole feed undecodable.

type FlexString string

func (s *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*s = ""
		return nil
	}
	if b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = FlexString(v)
		return nil
	}
	*s = FlexString(b)
	return nil
}

func (s FlexString) String() string { return string(s) }

type FlexFloat float64

func (f *FlexFloat) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	*f = 0
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return nil
		}
		if x, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			*f = FlexFloat(x)
		}
		return nil
	}
	var x float64
	if err := json.Unmarshal(b, &x); err == nil {
		*f = FlexFloat(x)
	}
	return nil
}

func (f FlexFloat) Float() float64 { return float64(f) }

type FlexInt int64

func (i *FlexInt) UnmarshalJSON(b []byte) error {
	var f FlexFloat
	if err := f.UnmarshalJSON(b); err != nil {
		return err
	}
	*i = FlexInt(f)
	return nil
}

// FlexTime accepts RFC 3339 strings and unix timestamps in seconds or milliseconds.
type FlexTime struct{ time.Time }

func (t *FlexTime) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	t.Time = time.Time{}
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return nil
		}
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(v)); err == nil {
			t.Time = parsed
			return nil
		}
		b = []byte(v)
	}
	if n, err := strconv.ParseFloat(string(b), 64); err == nil && n > 0 {
		if n > 1e12 {
			t.Time = time.UnixMilli(int64(n))
		} else {
			t.Time = time.Unix(int64(n), 0)
		}
	}
	return nil
}

type FlexBool bool

func (v *FlexBool) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch strings.ToLower(strings.Trim(string(b), `"`)) {
	case "true", "1", "yes":
		*v = true
	default:
		*v = false
	}
	return nil
}
