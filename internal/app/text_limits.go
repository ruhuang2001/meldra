package app

import (
	"strings"
	"unicode/utf8"
)

func truncateUTF8(value string, limit int, suffix string) string {
	if limit <= 0 {
		return ""
	}
	value = strings.ToValidUTF8(value, string(utf8.RuneError))
	if len(value) <= limit {
		return value
	}
	return truncateUTF8WithSuffix(value, limit, suffix)
}

func truncateUTF8WithSuffix(value string, limit int, suffix string) string {
	if limit <= 0 {
		return ""
	}
	value = strings.ToValidUTF8(value, string(utf8.RuneError))
	suffix = strings.ToValidUTF8(suffix, string(utf8.RuneError))
	if len(suffix) >= limit {
		return truncateUTF8Prefix(suffix, limit)
	}
	return truncateUTF8Prefix(value, limit-len(suffix)) + suffix
}

func truncateUTF8Prefix(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
