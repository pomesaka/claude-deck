package session

import "unicode"

// hasSpinnerPrefix checks if the title has a leading spinner character + space.
// Claude Code prefixes titles with Braille/dingbat spinner chars like "✳ ", "⠐ ".
func hasSpinnerPrefix(title string) bool {
	runes := []rune(title)
	if len(runes) < 2 {
		return false
	}
	r := runes[0]
	isBraille := r >= 0x2800 && r <= 0x28FF
	isDingbat := r >= 0x2700 && r <= 0x27BF
	isMiscSymbol := r >= 0x2600 && r <= 0x26FF
	isSpinner := isBraille || isDingbat || isMiscSymbol || (!unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ' ')
	return isSpinner && runes[1] == ' '
}

// stripSpinnerPrefix removes the leading spinner character + space from an OSC title.
func stripSpinnerPrefix(title string) string {
	if hasSpinnerPrefix(title) {
		return string([]rune(title)[2:])
	}
	return title
}

// containsBrailleSpinner checks if the line contains Braille Pattern characters (U+2800..U+28FF).
// Claude Code はローディング中に Braille スピナー（⠐⠑⠒ 等）を PTY に描画する。
func containsBrailleSpinner(line string) bool {
	for _, r := range line {
		if r >= 0x2800 && r <= 0x28FF {
			return true
		}
	}
	return false
}
