package driver

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Params is a driver-specific section of the config file as decoded from
// YAML. The helpers return errors whose text is meant for a club
// administrator, hence in Russian.
type Params map[string]any

// String returns a required or optional string value.
func (p Params) String(path, key string, required bool) (string, error) {
	v, ok := p[key]
	if !ok || v == nil {
		if required {
			return "", fmt.Errorf("%s.%s: параметр обязателен", path, key)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s.%s: должна быть строка", path, key)
	}
	s = strings.TrimSpace(s)
	if required && s == "" {
		return "", fmt.Errorf("%s.%s: параметр обязателен и не может быть пустым", path, key)
	}
	return s, nil
}

// Int returns a required integer value.
func (p Params) Int(path, key string) (int, error) {
	v, ok := p[key]
	if !ok || v == nil {
		return 0, fmt.Errorf("%s.%s: параметр обязателен", path, key)
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		if n > math.MaxInt32 || n < math.MinInt32 {
			return 0, fmt.Errorf("%s.%s: слишком большое число", path, key)
		}
		return int(n), nil
	case uint64:
		if n > math.MaxInt32 {
			return 0, fmt.Errorf("%s.%s: слишком большое число", path, key)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("%s.%s: должно быть целое число", path, key)
	}
}

// RejectUnknown returns an error naming every key not in allowed.
func (p Params) RejectUnknown(path string, allowed ...string) error {
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	var unknown []string
	for k := range p {
		if !ok[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("%s: неизвестные параметры: %s (допустимы: %s)",
		path, strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}
