// Package ui holds shared terminal styling so the CLI and TUI output match.
package ui

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// lipgloss renders unconditionally: Render emits ANSI whatever the destination is, and it
// is the WRITER that downgrades to what the destination can show. rackctl's styled strings
// reach the terminal through fmt.Println and through caller-supplied io.Writers, neither of
// which downgrades anything, so nothing between Render and a redirected file would strip an
// escape sequence.
//
// The profile is therefore resolved once, here, against the same stdout and environment the
// output goes to, and the palette below is the identity when that destination cannot show
// colour. Callers render the same way either way.
//
// This is a property the CLI depends on rather than a preference: every status glyph is also
// a distinct character precisely because colour is expected to vanish when output is piped,
// and a log file full of escape sequences is a different artifact from the one operators grep.
var colorized = lipgloss.Writer.Profile >= colorprofile.ANSI

// style returns s where the destination shows colour and an unstyled style otherwise.
// Attributes are composed through it rather than chained onto the exported values, so a
// disabled palette cannot have bold or underline reapplied on top of it.
func style(s lipgloss.Style) lipgloss.Style { return styleWhen(colorized, s) }

// styleWhen is the decision itself, separated from the ambient profile so it can be
// exercised for both answers. Reading the flag from the real stdout would make the test
// assert whatever the machine running it happens to be.
func styleWhen(colorized bool, s lipgloss.Style) lipgloss.Style {
	if colorized {
		return s
	}
	return lipgloss.NewStyle()
}

// Semantic palette — ANSI indices, so it inherits the user's terminal theme.
// Exported for reuse by the TUI; the helpers below compose them for the CLI.
var (
	Green  = style(lipgloss.NewStyle().Foreground(lipgloss.Color("2")))
	Red    = style(lipgloss.NewStyle().Foreground(lipgloss.Color("1")))
	Yellow = style(lipgloss.NewStyle().Foreground(lipgloss.Color("3")))
	Gray   = style(lipgloss.NewStyle().Foreground(lipgloss.Color("8")))
	Blue   = style(lipgloss.NewStyle().Foreground(lipgloss.Color("4")))
	Bold   = style(lipgloss.NewStyle().Bold(true))

	// The emphasised variants the helpers below render their glyph in. Built here rather
	// than chained at the call site for the reason style gives.
	greenBold = style(lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true))
	redBold   = style(lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true))
	blueBold  = style(lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true))
	titled    = style(lipgloss.NewStyle().Bold(true).Underline(true))
)

func OK(s string) string    { return greenBold.Render("✓") + " " + s }
func Fail(s string) string  { return redBold.Render("✗") + " " + s }
func Step(s string) string  { return blueBold.Render("▸") + " " + s }
func Skip(s string) string  { return Gray.Render("• " + s) }
func Warn(s string) string  { return Yellow.Render("! " + s) }
func Title(s string) string { return titled.Render(s) }
