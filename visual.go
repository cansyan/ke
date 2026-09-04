package main

import (
	"unicode/utf8"
	
	"github.com/mattn/go-runewidth"
)

// ByteOffsetToVisualCol converts a 0-indexed byte offset within a line
// to its rendered visual display column, accounting for tab width and character cell widths.
func ByteOffsetToVisualCol(line []byte, byteOffset int, tabWidth int) int {
	col := 0
	currByte := 0

	for currByte < byteOffset && currByte < len(line) {
		r, size := utf8.DecodeRune(line[currByte:])
		if r == '\t' {
			col += tabWidth - (col % tabWidth)
		} else {
			col += runewidth.RuneWidth(r)
		}
		currByte += size
	}
	return col
}

// VisualColToByteOffset converts a visual display column
// back to the closest 0-indexed byte offset on line.
func VisualColToByteOffset(line []byte, visualCol int, tabWidth int) int {
	if len(line) == 0 || visualCol <= 0 {
		return 0
	}

	vCol := 0
	byteIdx := 0

	for byteIdx < len(line) {
		r, size := utf8.DecodeRune(line[byteIdx:])

		var runeWidth int
		if r == '\t' {
			runeWidth = tabWidth - (vCol % tabWidth)
		} else {
			runeWidth = runewidth.RuneWidth(r)
		}

		// Stop if advancing past visualCol
		if vCol+runeWidth > visualCol {
			// Snap to whichever side is closer (or return current byteIdx)
			break
		}

		vCol += runeWidth
		byteIdx += size
	}

	return byteIdx
}
