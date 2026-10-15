package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Align CJK labels by terminal cells rather than UTF-8 bytes or rune count.
// Rows use tabs as column separators; values are sanitized before rendering.
type cliTable struct {
	bytes.Buffer
	out io.Writer
}

func newCLITable(out io.Writer) *cliTable { return &cliTable{out: out} }
func (table *cliTable) Flush() error {
	lines := strings.Split(strings.TrimSuffix(table.String(), "\n"), "\n")
	rows := make([][]string, len(lines))
	widths := []int{}
	for i, line := range lines {
		rows[i] = strings.Split(line, "\t")
		for col, value := range rows[i] {
			if col >= len(widths) {
				widths = append(widths, 0)
			}
			if n := cliTextWidth(value); n > widths[col] {
				widths[col] = n
			}
		}
	}
	for _, row := range rows {
		for col, value := range row {
			if _, err := io.WriteString(table.out, value); err != nil {
				return err
			}
			if col < len(row)-1 {
				if _, err := io.WriteString(table.out, strings.Repeat(" ", widths[col]-cliTextWidth(value)+2)); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintln(table.out); err != nil {
			return err
		}
	}
	table.Reset()
	return nil
}
func cliTextWidth(value string) int {
	width := 0
	for _, r := range value {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d {
			continue
		}
		if r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a || (r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) || (r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe10 && r <= 0xfe19) || (r >= 0xfe30 && r <= 0xfe6f) || (r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6) || (r >= 0x1f300 && r <= 0x1faff) || (r >= 0x20000 && r <= 0x3fffd)) {
			width += 2
		} else {
			width++
		}
	}
	return width
}
