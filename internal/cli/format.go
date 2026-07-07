package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
)

// --- ANSI helpers (no external dependencies) ---------------------------------

var useColor = isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())

const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
)

func green(s string) string {
	if !useColor {
		return s
	}
	return ansiGreen + s + ansiReset
}

func red(s string) string {
	if !useColor {
		return s
	}
	return ansiRed + s + ansiReset
}

func yellow(s string) string {
	if !useColor {
		return s
	}
	return ansiYellow + s + ansiReset
}

func cyan(s string) string {
	if !useColor {
		return s
	}
	return ansiCyan + s + ansiReset
}

func bold(s string) string {
	if !useColor {
		return s
	}
	return ansiBold + s + ansiReset
}

func dim(s string) string {
	if !useColor {
		return s
	}
	return ansiDim + s + ansiReset
}

func colorStatus(status string) string {
	switch status {
	case "healthy":
		return green(status)
	case "unhealthy":
		return red(status)
	case "unknown":
		return yellow(status)
	case "working", "submitted":
		return cyan(status)
	case "completed":
		return green(status)
	case "failed":
		return red(status)
	case "canceled":
		return yellow(status)
	case "input-required":
		return yellow(status)
	default:
		return status
	}
}

// formatTimeShort converts an RFC3339 timestamp to a shorter human-readable form.
// Returns "" for empty input.
func formatTimeShort(ts string) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t2, err2 := time.Parse(time.RFC3339, ts)
		if err2 != nil {
			return ts
		}
		t = t2
	}
	now := time.Now().UTC()
	if now.Sub(t) < 24*time.Hour {
		return t.Format("15:04:05")
	}
	return t.Format("Jan 02 15:04")
}

// formatTimeSince returns a human-friendly "ago" string.
func formatTimeSince(ts string) string {
	if ts == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t2, err2 := time.Parse(time.RFC3339, ts)
		if err2 != nil {
			return ts
		}
		t = t2
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// shortID returns the first 8 chars of a UUID-style id.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// padRight pads a string to width with spaces.
func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// separator returns a line of the given character.
func separator(width int) string {
	return strings.Repeat("─", width)
}
