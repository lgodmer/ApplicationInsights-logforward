package common

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
)

type unescapeCallback func(string, *bytes.Buffer) bool
type unwrapCallback func(string) string

type ParserOptions struct {
	VariableRegex  string
	EscapeRegex    string
	Unescape       unescapeCallback
	UnwrapVariable unwrapCallback
}

type Parser struct {
	escapeRE         *regexp.Regexp
	segments         []*parserSegment
	unescapeCallback unescapeCallback
}

type parserSegment struct {
	variable string
	searcher *stringSearcher
}

type ParserResultStorage interface {
	Store(key, value string)
}

var NO_MATCH error = errors.New("Match not found")

func NewParser(format string, options *ParserOptions) (*Parser, error) {
	// Compile variable regexp
	varRE, err := regexp.Compile(options.VariableRegex)
	if err != nil {
		return nil, err
	}

	escRE, err := regexp.Compile(options.EscapeRegex)
	if err != nil {
		return nil, err
	}

	// Split format into variables/separator
	segments := splitSegments(format, varRE)

	// Combine var-sep-var-sep-... sequence into []*parserSegment
	// We'll compile (Boyer-Moore) the separators and cache those results since they're
	// probably reused.
	var psegments []*parserSegment
	var variable string
	searchers := make(map[string]*stringSearcher)
	for _, segment := range segments {
		if varRE.MatchString(segment) {
			// Variable
			if variable != "" {
				// Two consecutive variables
				return nil, fmt.Errorf("Two consecutive variables in format: %s, %s", variable, segment)
			}

			variable = segment
		} else {
			// Separator
			searcher, ok := searchers[segment]
			if !ok {
				searcher = compileSearcher(segment)
				searchers[segment] = searcher
			}

			varname := variable
			if variable != "" {
				varname = options.UnwrapVariable(variable)
			}

			psegments = append(psegments, &parserSegment{
				variable: varname,
				searcher: searcher,
			})

			variable = ""
		}
	}

	if variable != "" {
		// If we ended with a variable, then add a parser segment for it.
		psegments = append(psegments, &parserSegment{
			variable: options.UnwrapVariable(variable),
			searcher: nil,
		})
	}

	return &Parser{
		escapeRE:         escRE,
		segments:         psegments,
		unescapeCallback: options.Unescape,
	}, nil
}

func splitSegments(format string, varRE *regexp.Regexp) []string {
	var segments []string

	for len(format) > 0 {
		loc := varRE.FindStringIndex(format)
		if loc == nil {
			segments = append(segments, format)
			break
		} else {
			if loc[0] > 0 {
				segments = append(segments, format[:loc[0]])
			}

			segments = append(segments, format[loc[0]:loc[1]])
			format = format[loc[1]:]
		}
	}

	return segments
}

func (parser *Parser) Parse(line string, output ParserResultStorage) error {
	// First, find all of the escape sequences in the input so we can skip over them
	// when processing the line.
	escapes := parser.escapeRE.FindAllStringIndex(line, -1)

	ptr := 0
	for _, segment := range parser.segments {
		if segment.variable == "" {
			// Look for a delimiter at the beginning, don't read into a variable
			_, eidx, escidx, err := segment.searcher.Search(line, ptr, escapes)
			if err != nil {
				return err
			}

			ptr = eidx
			escapes = escapes[escidx:]
		} else if segment.searcher == nil {
			// Read the rest of the line into a variable
			value := line[ptr:]
			if len(escapes) > 0 {
				value = parser.unescape(value, ptr, escapes)
			}

			output.Store(segment.variable, value)
			ptr = len(line)
		} else {
			// Find separator,
			idx, eidx, escidx, err := segment.searcher.Search(line, ptr, escapes)
			if err != nil {
				return err
			}

			// Unescape the value only if we skipped over any escapes
			value := line[ptr:idx]
			if escidx > 0 {
				value = parser.unescape(value, ptr, escapes[0:escidx])
				escapes = escapes[escidx:]
			}

			output.Store(segment.variable, value)
			ptr = eidx
		}
	}

	return nil
}

func (parser *Parser) ParseToMap(line string) (map[string]string, error) {
	result := make(parserResultMap)
	err := parser.Parse(line, result)
	if err != nil {
		return nil, err
	} else {
		return result, nil
	}
}

type parserResultMap map[string]string

func (m parserResultMap) Store(key, value string) {
	m[key] = value
}

func (parser *Parser) unescape(match string, offset int, escapes [][]int) string {
	var buf bytes.Buffer
	last := 0

	for _, esc := range escapes {
		// Get escape relative to offset
		escStart := esc[0] - offset
		escEnd := esc[1] - offset

		// Write last-escape start into buffer
		buf.WriteString(match[last:escStart])

		// Unescape into buffer
		parser.unescapeCallback(match[escStart:escEnd], &buf)

		// Advance last pointer
		last = escEnd
	}

	// Write remainder of string
	buf.WriteString(match[last:])

	return buf.String()
}

type stringSearcher struct {
	pattern      string
	badChars     [256]int
	goodSuffixes []int
}

func compileSearcher(pattern string) *stringSearcher {
	result := &stringSearcher{pattern: pattern}
	length := len(pattern)
	last := length - 1

	// Bad character rule
	for i := 0; i < 256; i++ {
		result.badChars[i] = length
	}
	for i := 0; i < length; i++ {
		result.badChars[pattern[i]] = last - i
	}

	// Good suffix rule - http://www-igm.univ-mlv.fr/~lecroq/string/node14.html

	// For each position i, pattern[:i+1] has the same suffix as pattern for suffixes[i] bytes,
	// or: pattern[i-suffixes[i]+1:i+1] == pattern[len(pattern)-suffixes[i]:]
	suffixes := make([]int, length)

	g := last
	f := last - 1
	suffixes[last] = length

	for i := last - 1; i >= 0; i-- {
		if i > g && suffixes[i+last-f] < i-g {
			suffixes[i] = suffixes[i+last-f]
		} else {
			if i < g {
				g = i
			}
			f = i
			for g >= 0 && pattern[g] == pattern[g+last-f] {
				g--
			}
			suffixes[i] = f - g
		}
	}

	// Build jump table based on matching suffixes, above.

	result.goodSuffixes = make([]int, length)
	for i := 0; i < length; i++ {
		result.goodSuffixes[i] = length
	}

	j := 0
	for i := last; i >= 0; i-- {
		if suffixes[i] == i+1 {
			for ; j < last-i; j++ {
				if result.goodSuffixes[j] == length {
					result.goodSuffixes[j] = last - i
				}
			}
		}
	}

	for i := 0; i < last; i++ {
		result.goodSuffixes[last-suffixes[i]] = last - i
	}

	return result
}

func (search *stringSearcher) Search(line string, start int, escapes [][]int) (int, int, int, error) {
	escidx := 0

	for i := start; i <= len(line)-len(search.pattern); {
		j := len(search.pattern) - 1

		// Skip over escapes we've already passed
		for escidx < len(escapes) && escapes[escidx][1] <= i {
			escidx++
		}

		// Skip i over the next escape if we're in the middle of it
		if escidx < len(escapes) && escapes[escidx][0] <= (j+i) {
			i = escapes[escidx][1]
			continue
		}

		// Perform check
		for j >= 0 && search.pattern[j] == line[i+j] {
			j--
		}
		if j < 0 {
			// Matched
			return i, i + len(search.pattern), escidx, nil
		}

		// No match
		bc := search.badChars[line[i+j]] - len(search.pattern) + 1 + j
		gs := search.goodSuffixes[j]

		if bc > gs {
			i += bc
		} else {
			i += gs
		}
	}

	return 0, 0, 0, NO_MATCH
}
