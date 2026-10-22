package cli

import "strings"

// ParseShow turns the console output of a `show` command into a tree like
// the RCI JSON answer. The console right-aligns keys, so the nesting level
// is the column of the colon: a key with an empty value opens an object
// whose members have their colon further right; a repeated key becomes a
// list.
func ParseShow(text string) map[string]any {
	type level struct {
		indent int
		m      map[string]any
	}
	root := map[string]any{}
	stack := []level{{-1, root}}
	var pendingKey string
	pendingIndent := -1
	var pendingParent map[string]any
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimLeft(line, " \t")
		key, val, ok := strings.Cut(trimmed, ":")
		if !ok || key == "" || strings.Contains(key, " ") {
			continue
		}
		indent := len(line) - len(trimmed) + len(key)
		val = strings.TrimSpace(val)
		if pendingKey != "" {
			child := map[string]any{}
			if indent > pendingIndent {
				put(pendingParent, pendingKey, child)
				stack = append(stack, level{pendingIndent, child})
			} else {
				put(pendingParent, pendingKey, "")
			}
			pendingKey = ""
		}
		for len(stack) > 1 && indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].m
		if val == "" {
			pendingKey, pendingIndent, pendingParent = key, indent, parent
			continue
		}
		put(parent, key, val)
	}
	if pendingKey != "" {
		put(pendingParent, pendingKey, "")
	}
	return root
}

func put(m map[string]any, k string, v any) {
	old, ok := m[k]
	if !ok {
		m[k] = v
		return
	}
	if list, ok := old.([]any); ok {
		m[k] = append(list, v)
		return
	}
	m[k] = []any{old, v}
}
