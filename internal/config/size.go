package config

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// DiskSize is a byte count that accepts human friendly values such as
// "4GB", "20G" or plain numbers (bytes) in JSON.
type DiskSize int64

// UnmarshalJSON implements json.Unmarshaler.
func (d *DiskSize) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case string:
		n, err := ParseDiskSize(t)
		if err != nil {
			return err
		}
		*d = n
	case float64:
		*d = DiskSize(int64(t))
	default:
		return fmt.Errorf("invalid disk size %v", v)
	}
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d DiskSize) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// String renders the size with a binary unit suffix when possible.
func (d DiskSize) String() string {
	n := int64(d)
	switch {
	case n == 0:
		return "0"
	case n%(1<<30) == 0:
		return strconv.FormatInt(n/(1<<30), 10) + "G"
	case n%(1<<20) == 0:
		return strconv.FormatInt(n/(1<<20), 10) + "M"
	case n%(1<<10) == 0:
		return strconv.FormatInt(n/(1<<10), 10) + "K"
	default:
		return strconv.FormatInt(n, 10)
	}
}

// ParseDiskSize parses values such as "20", "20GB", "50G", "512M".
// Plain numbers are interpreted as bytes.
func ParseDiskSize(s string) (DiskSize, error) {
	t := strings.TrimSpace(strings.ToUpper(s))
	if t == "" {
		return 0, fmt.Errorf("empty disk size")
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(t, "KIB"):
		mult, t = 1<<10, strings.TrimSuffix(t, "KIB")
	case strings.HasSuffix(t, "MIB"):
		mult, t = 1<<20, strings.TrimSuffix(t, "MIB")
	case strings.HasSuffix(t, "GIB"):
		mult, t = 1<<30, strings.TrimSuffix(t, "GIB")
	case strings.HasSuffix(t, "TIB"):
		mult, t = 1<<40, strings.TrimSuffix(t, "TIB")
	case strings.HasSuffix(t, "KB"):
		mult, t = 1<<10, strings.TrimSuffix(t, "KB")
	case strings.HasSuffix(t, "MB"):
		mult, t = 1<<20, strings.TrimSuffix(t, "MB")
	case strings.HasSuffix(t, "GB"):
		mult, t = 1<<30, strings.TrimSuffix(t, "GB")
	case strings.HasSuffix(t, "TB"):
		mult, t = 1<<40, strings.TrimSuffix(t, "TB")
	case strings.HasSuffix(t, "K"):
		mult, t = 1<<10, strings.TrimSuffix(t, "K")
	case strings.HasSuffix(t, "M"):
		mult, t = 1<<20, strings.TrimSuffix(t, "M")
	case strings.HasSuffix(t, "G"):
		mult, t = 1<<30, strings.TrimSuffix(t, "G")
	case strings.HasSuffix(t, "T"):
		mult, t = 1<<40, strings.TrimSuffix(t, "T")
	case strings.HasSuffix(t, "B"):
		t = strings.TrimSuffix(t, "B")
	}
	t = strings.TrimSpace(t)
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid disk size %q", s)
	}
	v := n * float64(mult)
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("disk size %q is too large", s)
	}
	return DiskSize(int64(v)), nil
}
