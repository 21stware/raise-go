package appkit

import (
	"regexp"
	"strings"
)

// CSV exports and imports of the apps (biz, resource). A spreadsheet runs a cell that starts with = + - @ (or a tab or a carriage
// return in front of one) as a formula: an exported name like =HYPERLINK(...) would become a live link, +cmd|... a command.
// CSVSafe puts an apostrophe in front of such a cell (spreadsheets show the text as is and hide the apostrophe); a plain number
// (-12.50, +3) is left alone so it stays a number. CSVUnescape is its exact inverse for an import: a file exported here and
// imported again gives back the same values. The rule is a bijection because a value that already starts with an apostrophe
// followed by something CSVSafe would escape is escaped once more ('=x → ''=x).

var plainNumber = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?$`)

// csvNeedsEscape reports whether CSVSafe would escape v.
func csvNeedsEscape(v string) bool {
	if v == "" {
		return false
	}
	switch v[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return !plainNumber.MatchString(v)
	case '\'':
		return csvNeedsEscape(v[1:])
	}
	return false
}

// CSVSafe is one exported cell that a spreadsheet will not run as a formula.
func CSVSafe(v string) string {
	if csvNeedsEscape(v) {
		return "'" + v
	}
	return v
}

// CSVSafeRow applies CSVSafe to every cell of a row (in place) and returns it.
func CSVSafeRow(row []string) []string {
	for i, v := range row {
		row[i] = CSVSafe(v)
	}
	return row
}

// CSVUnescape undoes CSVSafe on one imported cell. An apostrophe a person typed in front of ordinary text is kept: the import
// stores values as they are, and only the export escapes.
func CSVUnescape(v string) string {
	if strings.HasPrefix(v, "'") && csvNeedsEscape(v[1:]) {
		return v[1:]
	}
	return v
}
