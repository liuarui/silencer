package gateway

import (
	"strings"
	"testing"
)

func TestToolPlaceholderIdentifierBoundaries(t *testing.T) {
	for _, text := range []string{
		"prefixx_bcdfghsuffix",
		"ordinary_variable_0123456789abcdefZ",
		"prefixIP_PUBLIC_ffffffffffffffff",
		"IP_PUBLIC_ffffffffffffffffx",
		"{{IP_PUBLIC_ffffffffffffffffx}}",
		"TOO_LONG_LABEL_ffffffffffffffff",
	} {
		if hasUnresolvedToolPlaceholder(text) {
			t.Errorf("ordinary identifier was treated as a placeholder: %q", text)
		}
		for cut := 0; cut <= len(text); cut++ {
			c := protocolChannel{toolInput: true}
			if err := c.checkArguments([]byte(text[:cut]), false); err != nil {
				t.Fatalf("cut %d prematurely rejected %q: %v", cut, text, err)
			}
			if err := c.checkArguments([]byte(text[cut:]), true); err != nil {
				t.Fatalf("cut %d rejected complete identifier %q: %v", cut, text, err)
			}
		}
	}
	for _, text := range []string{
		"IP_PUBLIC_ffffffffffffffff",
		"{{IP_PUBLIC_ffffffffffffffff}}",
		"x{{IP_PUBLIC_ffffffffffffffff}}",
		`\{\{IP_PUBLIC_ffffffffffffffff\}\}`,
		"{{ IP_PUBLIC_ ffffffffffffffff }}",
		"{{_SECRET_abc123}}",
		"{{SECRET_zzzzzz}}",
	} {
		if !hasUnresolvedToolPlaceholder(text) {
			t.Errorf("unresolved placeholder was accepted: %q", text)
		}
	}
}

func TestToolPlaceholderWaitsForRightBoundary(t *testing.T) {
	for _, padding := range []string{"", strings.Repeat(" ", 64)} {
		for _, ending := range []string{"", " ", "\"}"} {
			c := protocolChannel{toolInput: true}
			text := strings.Repeat("x", 300) + " IP_PUBLIC" + padding + "_" + padding + "ffffffffffffffff"
			if err := c.checkArguments([]byte(text), false); err != nil {
				t.Fatal("a delta ending is not an identifier boundary:", err)
			}
			if err := c.checkArguments([]byte(ending), true); err == nil {
				t.Errorf("unresolved placeholder escaped at completion with ending %q, padding %d", ending, len(padding))
			}
		}
	}
	// The tail may start inside a long random identifier. Trimming must not
	// turn that substring into an independent placeholder on the next delta.
	text := strings.Repeat("x", 20) + "x_bcdfghZ" + strings.Repeat("x", restoreTokenTailLimit-8)
	c := protocolChannel{toolInput: true}
	if err := c.checkArguments([]byte(text), false); err != nil {
		t.Fatal(err)
	}
	if err := c.checkArguments(nil, true); err != nil {
		t.Fatal("tail trimming invented a placeholder:", err)
	}
}
