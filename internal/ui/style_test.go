package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

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
//
// Asserted against the TEXT rather than the rendered bytes. Underline is encoded per
// character, so a styled title carries an escape sequence between every rune and the
// message is not a substring of it — matching raw would make this assert the encoding
// rather than the message, and pass or fail on whether the machine running it has a
// colour-capable terminal.
func TestHelpers_PreserveTheMessage(t *testing.T) {
	const msg = "network applied"
	for name, rendered := range map[string]string{
		"OK": OK(msg), "Fail": Fail(msg), "Step": Step(msg),
		"Skip": Skip(msg), "Warn": Warn(msg), "Title": Title(msg),
	} {
		if text := unstyle(rendered); !contains(text, msg) {
			t.Errorf("%s dropped its message: %q (text %q)", name, rendered, text)
		}
	}
}

// unstyle removes SGR escape sequences so an assertion reads the text a person sees.
func unstyle(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// The palette is the identity where the destination cannot show colour.
//
// lipgloss renders unconditionally — Render emits ANSI regardless of what it is writing to,
// and downgrading is the writer's job. rackctl's styled strings go to fmt.Println and to
// caller-supplied io.Writers, so no writer downgrades them and the escape sequences would
// reach a redirected file intact. Both answers are asserted: styling that never applies is
// as wrong as styling that never stops.
func TestStyleWhen_AppliesColourOnlyWhereItCanBeShown(t *testing.T) {
	styled := lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)

	if got := styleWhen(false, styled).Render("X"); got != "X" {
		t.Errorf("with colour unavailable, Render = %q, want %q — these bytes land in "+
			"whatever file the operator redirected output to", got, "X")
	}
	if got := styleWhen(true, styled).Render("X"); got == "X" {
		t.Error("with colour available, Render returned unstyled text — the palette " +
			"is disabled unconditionally rather than by destination")
	}
}

// The helpers agree with the resolved profile. TestStyleWhen pins the decision; this pins
// that the exported surface is actually wired to it, which a correct decision applied to
// nothing would otherwise satisfy.
func TestHelpers_AgreeWithTheResolvedProfile(t *testing.T) {
	for name, rendered := range map[string]string{
		"OK": OK("x"), "Fail": Fail("x"), "Step": Step("x"),
		"Skip": Skip("x"), "Warn": Warn("x"), "Title": Title("x"),
	} {
		esc := strings.ContainsRune(rendered, 0x1b)
		if esc != colorized {
			t.Errorf("%s rendered %q: carries an escape sequence = %v, but the resolved "+
				"profile says colour is available = %v", name, rendered, esc, colorized)
		}
	}
}
