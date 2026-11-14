package importflow

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

type pathParts struct {
	volume string
	parts  []string
}

func cleanText(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

func parsePath(style PathStyle, path string) (pathParts, bool) {
	if !cleanText(path, 4096) {
		return pathParts{}, false
	}
	var result pathParts
	switch style {
	case UnixPath:
		if !strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
			return pathParts{}, false
		}
		result.volume = "/"
		path = strings.TrimPrefix(path, "/")
	case WindowsPath:
		path = strings.ReplaceAll(path, "\\", "/")
		switch {
		case strings.HasPrefix(path, "//"):
			parts := strings.Split(path[2:], "/")
			if len(parts) < 2 || !windowsComponent(parts[0]) || !windowsComponent(parts[1]) {
				return pathParts{}, false
			}
			result.volume = "//" + strings.ToLower(parts[0]+"/"+parts[1])
			path = strings.Join(parts[2:], "/")
		case len(path) >= 3 && isLetter(path[0]) && path[1:3] == ":/":
			result.volume, path = strings.ToLower(path[:2]), path[3:]
		default:
			return pathParts{}, false
		}
	default:
		return pathParts{}, false
	}
	if path == "" {
		return result, true
	}
	result.parts = strings.Split(strings.TrimSuffix(path, "/"), "/")
	for index, part := range result.parts {
		if part == "" || part == "." || part == ".." || (style == WindowsPath && !windowsComponent(part)) {
			return pathParts{}, false
		}
		if style == WindowsPath {
			result.parts[index] = strings.ToLower(part)
		}
	}
	return result, true
}

func windowsComponent(part string) bool {
	return part != "" && part != "." && part != ".." && !strings.ContainsAny(part, `:*?"<>|`) &&
		!strings.HasSuffix(part, ".") && !strings.HasSuffix(part, " ")
}

func samePath(style PathStyle, a, b string) bool {
	left, leftOK := parsePath(style, a)
	right, rightOK := parsePath(style, b)
	return leftOK && rightOK && left.volume == right.volume && slices.Equal(left.parts, right.parts)
}

func isLetter(b byte) bool { return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' }

func pathWithin(style PathStyle, parent, child string, allowEqual bool) bool {
	root, rootOK := parsePath(style, parent)
	file, fileOK := parsePath(style, child)
	return rootOK && fileOK && root.volume == file.volume && len(root.parts) <= len(file.parts) &&
		(allowEqual || len(root.parts) < len(file.parts)) && slices.Equal(root.parts, file.parts[:len(root.parts)])
}
