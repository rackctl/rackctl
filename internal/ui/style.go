// Package ui holds shared terminal styling so CLI and (future) TUI output match.
package ui

import "github.com/charmbracelet/lipgloss"

var (
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	gray   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	blue   = lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Bold(true)
	titled = lipgloss.NewStyle().Bold(true).Underline(true)
)

func OK(s string) string    { return green.Render("✓") + " " + s }
func Fail(s string) string  { return red.Render("✗") + " " + s }
func Step(s string) string  { return blue.Render("▸") + " " + s }
func Skip(s string) string  { return gray.Render("• " + s) }
func Warn(s string) string  { return yellow.Render("! " + s) }
func Title(s string) string { return titled.Render(s) }
