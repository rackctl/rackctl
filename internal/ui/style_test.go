package ui

import "testing"

// Every status carries a distinct text glyph, not colour alone.
//
// Colour is stripped whenever output is piped, redirected into a log, or read by someone
// who cannot distinguish the two ANSI indices this palette uses for success and failure.
// If OK and Fail differed only by colour, all three of those readers would see two
// identical lines — so the glyph is the accessible channel and this pins it.
func TestHelpers_CarryADistinctGlyphNotColourAlone(t *testing.T) {
	glyphs := map[string]string{
		"OK":   OK(""),
		"Fail": Fail(""),
		"Step": Step(""),
		"Skip": Skip(""),
		"Warn": Warn(""),
	}

	seen := map[rune]string{}
	for name, rendered := range glyphs {
		var glyph rune
		for _, r := range rendered {
			// Skip the ANSI control sequence the style wraps the glyph in.
			if r > 32 && r != '[' && r != ';' && r != 'm' && r != 27 && (r < '0' || r > '9') {
				glyph = r
				break
			}
		}
		if glyph == 0 {
			t.Errorf("%s renders no text glyph — with colour stripped it is indistinguishable "+
				"from every other status", name)
			continue
		}
		if other, dup := seen[glyph]; dup {
			t.Errorf("%s and %s share the glyph %q — colour is then the only thing telling "+
				"them apart", name, other, glyph)
		}
		seen[glyph] = name
	}
}

// The message survives the styling. A helper that dropped its argument would render a
// bare glyph, which reads as a status with no subject.
func TestHelpers_PreserveTheMessage(t *testing.T) {
	const msg = "network applied"
	for name, rendered := range map[string]string{
		"OK": OK(msg), "Fail": Fail(msg), "Step": Step(msg),
		"Skip": Skip(msg), "Warn": Warn(msg), "Title": Title(msg),
	} {
		if !contains(rendered, msg) {
			t.Errorf("%s dropped its message: %q", name, rendered)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
