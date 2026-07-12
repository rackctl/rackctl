// Package ui holds shared terminal styling so the CLI and TUI output match.
package ui

import "github.com/charmbracelet/lipgloss"

// Semantic palette — ANSI indices, so it inherits the user's terminal theme.
// Exported for reuse by the TUI; the helpers below compose them for the CLI.
var (
	Green  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	Red    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	Yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	Gray   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	Blue   = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	Bold   = lipgloss.NewStyle().Bold(true)
)

func OK(s string) string    { return Green.Bold(true).Render("✓") + " " + s }
func Fail(s string) string  { return Red.Bold(true).Render("✗") + " " + s }
func Step(s string) string  { return Blue.Bold(true).Render("▸") + " " + s }
func Skip(s string) string  { return Gray.Render("• " + s) }
func Warn(s string) string  { return Yellow.Render("! " + s) }
func Title(s string) string { return Bold.Underline(true).Render(s) }
