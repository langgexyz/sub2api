//go:build unit

package repository

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMatchExcerptRuneBoundary(t *testing.T) {
	// 命中点前是中文（多字节 rune）：idx-40 字节偏移大概率落在 rune 中间，
	// 修复前会切出以 U+FFFD 开头的乱码摘录。
	text := strings.Repeat("部署流程说明", 20) + "KEYWORD tail"
	got := matchExcerpt(text, "KEYWORD")
	if !utf8.ValidString(got) {
		t.Fatalf("excerpt is not valid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("excerpt starts mid-rune (contains U+FFFD): %q", got)
	}
	if !strings.Contains(got, "KEYWORD") {
		t.Fatalf("excerpt lost the match: %q", got)
	}

	// 纯 ASCII 前缀不受影响。
	ascii := strings.Repeat("x", 100) + "KEYWORD"
	if got := matchExcerpt(ascii, "KEYWORD"); !strings.Contains(got, "KEYWORD") {
		t.Fatalf("ascii excerpt lost the match: %q", got)
	}
}
