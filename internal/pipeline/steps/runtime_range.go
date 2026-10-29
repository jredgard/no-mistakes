package steps

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type nodeVersion [3]int

func (v nodeVersion) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }
func (v nodeVersion) Compare(other nodeVersion) int {
	for i := range v {
		if v[i] < other[i] {
			return -1
		}
		if v[i] > other[i] {
			return 1
		}
	}
	return 0
}

// Only stable numeric Node versions are selected. Manager aliases (lts/*,
// node, system), prereleases and unsupported range syntax fail explicitly;
// they cannot accidentally fall back to the host's active version.
func parseNodeVersion(raw string) (nodeVersion, int, error) {
	parts := strings.Split(strings.TrimPrefix(raw, "v"), ".")
	v := nodeVersion{}
	if len(parts) > 3 {
		return v, 0, fmt.Errorf("invalid Node version %q", raw)
	}
	count := 0
	wild := false
	for i, part := range parts {
		if part == "x" || part == "X" || part == "*" {
			wild = true
			continue
		}
		if wild || part == "" || strings.Trim(part, "0123456789") != "" || (len(part) > 1 && part[0] == '0') {
			return v, 0, fmt.Errorf("invalid Node version %q", raw)
		}
		n, err := strconv.Atoi(part)
		if err != nil || n > 1000000 {
			return v, 0, fmt.Errorf("invalid Node version %q", raw)
		}
		v[i] = n
		count++
	}
	return v, count, nil
}

var nodeComparator = regexp.MustCompile(`^(>=|<=|>|<|=|\^|~)?(.+)$`)
var nodeComparatorSpace = regexp.MustCompile(`(>=|<=|>|<|=|\^|~)\s+`)

// Implements stable numeric npm ranges: comparator sets, ||, ^, ~, partial
// versions and x/* wildcards. Reject other syntax rather than approximate it.
func compileNodeRange(raw string) (func(nodeVersion) bool, error) {
	var alternatives [][]func(nodeVersion) bool
	for _, alternative := range strings.Split(raw, "||") {
		fields := strings.Fields(nodeComparatorSpace.ReplaceAllString(alternative, "$1"))
		if len(fields) == 0 {
			return nil, fmt.Errorf("empty Node version range")
		}
		var predicates []func(nodeVersion) bool
		for _, field := range fields {
			parts := nodeComparator.FindStringSubmatch(field)
			if parts == nil {
				return nil, fmt.Errorf("unsupported Node range %q", raw)
			}
			lower, count, err := parseNodeVersion(parts[2])
			if err != nil {
				return nil, fmt.Errorf("unsupported Node range %q: %w", raw, err)
			}
			op := parts[1]
			if count == 0 {
				if op != "" && op != "=" {
					return nil, fmt.Errorf("unsupported wildcard comparator %q", field)
				}
				predicates = append(predicates, func(nodeVersion) bool { return true })
				continue
			}
			upper := lower
			index := count - 1
			if op == "^" {
				index = 0
				for index < count-1 && lower[index] == 0 {
					index++
				}
			} else if op == "~" && count > 1 {
				index = 1
			}
			upper[index]++
			for i := index + 1; i < 3; i++ {
				upper[i] = 0
			}
			predicates = append(predicates, func(v nodeVersion) bool {
				cmp := v.Compare(lower)
				switch op {
				case ">=":
					return cmp >= 0
				case ">":
					if count < 3 {
						return v.Compare(upper) >= 0
					}
					return cmp > 0
				case "<":
					return cmp < 0
				case "<=":
					if count < 3 {
						return v.Compare(upper) < 0
					}
					return cmp <= 0
				case "^", "~":
					return cmp >= 0 && v.Compare(upper) < 0
				default:
					if count == 3 {
						return cmp == 0
					}
					return cmp >= 0 && v.Compare(upper) < 0
				}
			})
		}
		alternatives = append(alternatives, predicates)
	}
	return func(v nodeVersion) bool {
		for _, predicates := range alternatives {
			matched := true
			for _, predicate := range predicates {
				if !predicate(v) {
					matched = false
					break
				}
			}
			if matched {
				return true
			}
		}
		return false
	}, nil
}
