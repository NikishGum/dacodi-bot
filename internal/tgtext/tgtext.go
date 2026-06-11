// Package tgtext holds text helpers shared by the Telegram-facing code paths.
package tgtext

import "strings"

// MaxRunes is a conservative per-message length used when splitting long
// operator replies. Telegram's hard limit is 4096 UTF-16 code units; 3500 runes
// stays safely under it for any input (FIX-4).
const MaxRunes = 3500

// SplitMessage splits s into chunks of at most maxRunes runes, breaking on rune
// boundaries so a multi-byte UTF-8 character is never severed (which Telegram
// rejects with a 400). Within each chunk it prefers to break at the last newline,
// then the last space, before the limit, falling back to a hard cut. Empty
// chunks are dropped; an all-whitespace input yields no chunks.
func SplitMessage(s string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = MaxRunes
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []string{s}
	}

	var parts []string
	for len(runes) > maxRunes {
		cut := preferredCut(runes, maxRunes)
		chunk := strings.TrimRight(string(runes[:cut]), " \t\r\n")
		if chunk != "" {
			parts = append(parts, chunk)
		}
		// Skip whitespace at the start of the continuation so a chunk does not
		// begin with the break character we split on.
		for cut < len(runes) && isBreakSpace(runes[cut]) {
			cut++
		}
		runes = runes[cut:]
	}
	if tail := strings.TrimRight(string(runes), " \t\r\n"); strings.TrimSpace(tail) != "" {
		parts = append(parts, tail)
	}
	return parts
}

// preferredCut returns the index (1..maxRunes) at which to split: the position
// just after the last newline within the window, else just after the last space,
// else a hard cut at maxRunes.
func preferredCut(runes []rune, maxRunes int) int {
	for i := maxRunes; i > 0; i-- {
		if runes[i-1] == '\n' {
			return i
		}
	}
	for i := maxRunes; i > 0; i-- {
		if runes[i-1] == ' ' {
			return i
		}
	}
	return maxRunes
}

func isBreakSpace(r rune) bool {
	return r == ' ' || r == '\n' || r == '\t' || r == '\r'
}
