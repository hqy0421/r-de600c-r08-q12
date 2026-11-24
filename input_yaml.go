package trdsql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/lexer"
	"github.com/goccy/go-yaml/token"
	"github.com/itchyny/gojq"
)

// YAMLReader parses YAML documents as tabular input.
//
// A YAML stream can contain several documents separated by "---".
// Every imported row keeps the 1-based index of its source document
// (when InYAMLDocument is enabled) and empty documents produce an
// all-NULL row so that the imported table can be reconciled against
// the registration file. When columns are pre-declared with
// InYAMLColumns, additional fields of later documents are handled
// according to InYAMLExtra.
type YAMLReader struct {
	query      *gojq.Query
	already    map[string]bool
	inNULL     string
	preRead    []map[string]any
	pending    []map[string]any
	preReadDoc []int
	pendingDoc []int
	names      []string
	types      []string
	limitRead  bool
	needNULL   bool

	// docColumn is true when the "doc" identity column is emitted.
	docColumn bool
	// docName is the (possibly collision-renamed) identity column name.
	docName string
	// declared holds the pre-declared column schema (nil = inferred).
	declared []string
	// extraMode decides how undeclared fields are handled.
	extraMode YAMLExtraMode
	// extraName is the JSON column name, renamed if a data column
	// already uses "extra".
	extraName string
}

// NewYAMLReader returns a YAMLReader configured with input options and jq filter.
func NewYAMLReader(reader io.Reader, opts *ReadOpts) (*YAMLReader, error) {
	query, err := jqParse(opts.InJQuery)
	if err != nil {
		return nil, err
	}

	r := &YAMLReader{
		query:     query,
		already:   make(map[string]bool),
		limitRead: opts.InLimitRead,
		needNULL:  opts.InNeedNULL,
		inNULL:    opts.InNULL,
		docColumn: opts.InYAMLDocument,
		declared:  opts.InYAMLColumns,
		extraMode: opts.InYAMLExtra,
	}

	// Fix the column order up front: the optional identity column is
	// reserved first, then the pre-declared schema (with the "extra"
	// JSON column reserved when enabled). In inferred mode data
	// columns are appended as documents are parsed; the identity
	// column is renamed if a data column later claims its name.
	if r.docColumn {
		r.docName = "doc"
		r.already[r.docName] = true
		r.names = append(r.names, r.docName)
	}
	if len(r.declared) > 0 {
		r.appendNames(r.declared)
		if opts.InYAMLExtra == YAMLExtraJSON {
			r.extraName = r.uniqueExtraName()
			r.appendNames([]string{r.extraName})
		}
	}

	if err := r.yamlParse(reader, opts); err != nil {
		return nil, err
	}

	return r, nil
}

// yamlSegments splits a YAML stream into the raw bytes of each
// document, preserving empty documents (including consecutive "---"
// separators). The goccy decoder silently drops empty documents, so
// document boundaries are taken from the lexer, whose DocumentHeader
// tokens report a rune offset for every "---" at column 1.
func yamlSegments(src []byte) [][]byte {
	tokens := lexer.Tokenize(string(src))

	var headerRuneOffsets []int
	for _, tk := range tokens {
		if tk.Type == token.DocumentHeaderType && tk.Position != nil {
			headerRuneOffsets = append(headerRuneOffsets, tk.Position.Offset)
		}
	}
	if len(headerRuneOffsets) == 0 {
		// A stream without document markers holds at most one document.
		// A completely blank stream holds no document at all.
		if hasYAMLContent(src) {
			return [][]byte{src}
		}
		return nil
	}

	// Token offsets are rune offsets, while slicing the source byte
	// slice needs byte offsets.
	headerByteOffsets := runeOffsetsToByteOffsets(src, headerRuneOffsets)

	var segments [][]byte
	if headerByteOffsets[0] > 0 && hasImplicitDocument(src[:headerByteOffsets[0]]) {
		// Content (even comments only) before the first header is an
		// implicit document; an all-whitespace prefix is not.
		segments = append(segments, src[:headerByteOffsets[0]])
	}
	for i, start := range headerByteOffsets {
		end := len(src)
		if i+1 < len(headerByteOffsets) {
			end = headerByteOffsets[i+1]
		}
		segments = append(segments, src[start:end])
	}
	return segments
}

// runeOffsetsToByteOffsets converts lexer rune offsets into byte
// offsets of the same source buffer. Lexer positions are 1-based
// (the first rune is at offset 1), so each reported offset minus one
// is the rune index. Invalid UTF-8 sequences decode as one RuneError
// rune, matching the lexer's rune counting.
func runeOffsetsToByteOffsets(src []byte, runeOffsets []int) []int {
	byteOffsets := make([]int, len(runeOffsets))
	runeIndex := 0
	target := 0
	for byteOffset := 0; byteOffset <= len(src) && target < len(runeOffsets); {
		for target < len(runeOffsets) && runeIndex == runeOffsets[target]-1 {
			byteOffsets[target] = byteOffset
			target++
		}
		if target >= len(runeOffsets) || byteOffset == len(src) {
			break
		}
		_, size := utf8.DecodeRune(src[byteOffset:])
		byteOffset += size
		runeIndex++
	}
	return byteOffsets
}

// hasYAMLContent reports whether the buffer contains anything other
// than blank lines, comments and document markers.
func hasYAMLContent(buf []byte) bool {
	for _, line := range strings.Split(string(buf), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "---" || trimmed == "..." {
			continue
		}
		return true
	}
	return false
}

// hasImplicitDocument reports whether a prefix before the first
// document header represents a document: any non-whitespace (including
// comments) counts, while a purely blank prefix does not.
func hasImplicitDocument(buf []byte) bool {
	return len(bytes.TrimSpace(buf)) > 0
}

// yamlParse reads every document of the stream and stores its rows in
// preRead (up to InPreRead documents worth of rows) and pending (the
// remaining rows, consumed by ReadRow). preReadDoc and pendingDoc hold
// the 1-based source document index of each row in the same order.
func (r *YAMLReader) yamlParse(reader io.Reader, opts *ReadOpts) error {
	src, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidYAML, err)
	}
	segments := yamlSegments(src)
	if len(segments) == 0 {
		// A completely blank stream has no documents to reconcile.
		return nil
	}

	docIndex := 0
	preDocs := 0
	for _, segment := range segments {
		docIndex++
		rows, err := r.parseDocument(segment, docIndex)
		if err != nil {
			return err
		}

		// An empty document still leaves an all-NULL reconciliation
		// row carrying the document identity.
		if len(rows) == 0 {
			rows = []map[string]any{{}}
		}

		// InPreRead counts documents, matching the previous behavior
		// where one Decode call read one whole document.
		if preDocs < opts.InPreRead {
			r.preRead = append(r.preRead, rows...)
			for range rows {
				r.preReadDoc = append(r.preReadDoc, docIndex)
			}
			preDocs++
		} else {
			r.pending = append(r.pending, rows...)
			for range rows {
				r.pendingDoc = append(r.pendingDoc, docIndex)
			}
		}
	}
	return nil
}

// parseDocument decodes one document segment and converts the
// top-level value into one or more tabular rows.
func (r *YAMLReader) parseDocument(segment []byte, docIndex int) ([]map[string]any, error) {
	if !hasYAMLContent(segment) {
		return nil, nil
	}

	var top any
	decoder := yaml.NewDecoder(bytes.NewReader(segment))
	if err := decoder.Decode(&top); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidYAML, err)
	}

	if r.query != nil {
		return r.jquery(top, docIndex)
	}
	return r.readAhead(top, docIndex)
}

// jquery applies the jq filter to one document.
func (r *YAMLReader) jquery(top any, docIndex int) ([]map[string]any, error) {
	var rows []map[string]any
	iter := r.query.Run(top)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			return nil, fmt.Errorf("%w gojq:(%s) ", err, r.query)
		}
		docRows, err := r.readAhead(v, docIndex)
		if err != nil {
			return nil, err
		}
		rows = append(rows, docRows...)
	}
	return rows, nil
}

// Names returns column names.
func (r *YAMLReader) Names() ([]string, error) {
	return r.names, nil
}

// Types returns column types.
// The document identity column is an int; all YAML values are text.
func (r *YAMLReader) Types() ([]string, error) {
	r.types = make([]string, len(r.names))
	for i, name := range r.names {
		if r.docColumn && name == r.docName {
			r.types[i] = "int"
			continue
		}
		r.types[i] = DefaultDBType
	}
	return r.types, nil
}

// uniqueExtraName returns the JSON extra column name, renamed if a
// declared column already uses "extra".
func (r *YAMLReader) uniqueExtraName() string {
	return r.uniqueName("extra")
}

// readAhead converts a top-level document value into tabular rows.
func (r *YAMLReader) readAhead(top any, docIndex int) ([]map[string]any, error) {
	var values []any
	switch m := top.(type) {
	case []any:
		values = m
	default:
		values = []any{top}
	}

	rows := make([]map[string]any, 0, len(values))
	for _, v := range values {
		row, names, err := r.topLevel(v)
		if err != nil {
			return nil, err
		}
		projected, err := r.projectRow(row, names)
		if err != nil {
			return nil, err
		}
		r.registerNames(projected)
		rows = append(rows, projected)
	}
	return rows, nil
}

// registerNames registers every not-yet-seen column name. Pre-declared
// names are registered up front, so this only grows the schema in
// inferred mode (or with YAMLExtraJSON's "extra" column).
func (r *YAMLReader) registerNames(row map[string]any) {
	names := make([]string, 0, len(row))
	for name := range row {
		names = append(names, name)
	}
	r.appendNames(names)
}

// appendNames adds multiple names for the argument to be unique.
// In inferred mode a data field may claim the identity column's
// name (even after a previous rename): the identity column is moved
// to a fresh name and the data field keeps its own name.
func (r *YAMLReader) appendNames(names []string) {
	for _, name := range names {
		if r.docColumn && name == r.docName {
			r.renameDocColumn()
		} else if r.already[name] {
			continue
		}
		r.already[name] = true
		r.names = append(r.names, name)
	}
}

// renameDocColumn moves the identity column to a collision-free name,
// replacing its reservation in place so column order is preserved.
// The new name is chosen while the old reservation is still held, so
// it cannot equal any real data column registered so far.
func (r *YAMLReader) renameDocColumn() {
	newName := r.uniqueName("doc")
	delete(r.already, r.docName)
	for i, name := range r.names {
		if name == r.docName {
			r.names[i] = newName
			break
		}
	}
	r.docName = newName
	r.already[newName] = true
}

// uniqueName returns a name based on prefix that is not in use in the
// already-set.
func (r *YAMLReader) uniqueName(prefix string) string {
	if !r.already[prefix] {
		return prefix
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s%d", prefix, i)
		if !r.already[candidate] {
			return candidate
		}
	}
}

// projectRow applies the pre-declared schema, if any. In inferred
// mode the row passes through unchanged and later documents keep
// adding columns as they are discovered.
func (r *YAMLReader) projectRow(row map[string]any, names []string) (map[string]any, error) {
	if len(r.declared) == 0 {
		return row, nil
	}

	result := make(map[string]any, len(r.declared)+1)
	for _, name := range r.declared {
		result[name] = row[name] // nil for documents missing the field
	}

	known := make(map[string]bool, len(r.declared))
	for _, name := range r.declared {
		known[name] = true
	}

	var extra map[string]any
	for _, name := range names {
		if known[name] {
			continue
		}
		switch r.extraMode {
		case YAMLExtraNull:
			// Undeclared fields are discarded.
			continue
		case YAMLExtraJSON:
			if extra == nil {
				extra = make(map[string]any)
			}
			extra[name] = row[name]
		default:
			return nil, fmt.Errorf("%w: YAML field %q is not in the pre-declared columns %v",
				ErrInvalidColumn, name, r.declared)
		}
	}

	if extra != nil {
		b, err := json.Marshal(extra)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrUnableConvert, err)
		}
		result[r.extraName] = string(b)
	}
	return result, nil
}

func (r *YAMLReader) topLevel(top any) (map[string]any, []string, error) {
	switch obj := top.(type) {
	case map[string]any:
		return r.objectRow(obj)
	case yaml.MapSlice:
		return r.objectMapSlice(obj)
	default:
		return r.etcRow(obj)
	}
}

// PreReadRow returns only columns that store preread rows.
func (r *YAMLReader) PreReadRow() [][]any {
	rows := make([][]any, len(r.preRead))
	for n, v := range r.preRead {
		rows[n] = r.materialize(v, r.preReadDoc[n])
	}
	return rows
}

// ReadRow reads the rest of the row.
func (r *YAMLReader) ReadRow(row []any) ([]any, error) {
	if r.limitRead {
		return nil, io.EOF
	}
	if len(r.pending) == 0 {
		return nil, io.EOF
	}
	next := r.pending[0]
	doc := r.pendingDoc[0]
	r.pending = r.pending[1:]
	r.pendingDoc = r.pendingDoc[1:]
	return r.materializeInto(row, next, doc), nil
}

// materialize renders a stored row map onto a fresh slice ordered by
// the current column names.
func (r *YAMLReader) materialize(stored map[string]any, doc int) []any {
	row := make([]any, len(r.names))
	return r.fill(row, stored, doc)
}

// materializeInto reuses the caller-provided row slice, growing it if
// the schema expanded since it was allocated.
func (r *YAMLReader) materializeInto(row []any, stored map[string]any, doc int) []any {
	if cap(row) < len(r.names) {
		row = make([]any, len(r.names))
	} else {
		row = row[:len(r.names)]
	}
	return r.fill(row, stored, doc)
}

// fill writes the stored map values into the row in column order.
func (r *YAMLReader) fill(row []any, stored map[string]any, doc int) []any {
	for i, name := range r.names {
		if r.docColumn && name == r.docName {
			row[i] = doc
			continue
		}
		value, ok := stored[name]
		if !ok {
			row[i] = nil
			continue
		}
		if r.needNULL {
			row[i] = replaceNULL(r.inNULL, value)
			continue
		}
		row[i] = value
	}
	return row
}

// objectRow returns a map of the YAML object and the column names.
func (r *YAMLReader) objectRow(obj map[string]any) (map[string]any, []string, error) {
	names := make([]string, 0, len(obj))
	row := make(map[string]any)
	for k, v := range obj {
		names = append(names, k)
		row[k] = r.toString(v)
	}
	return row, names, nil
}

// objectMapSlice returns a yaml.MapSlice of the YAML object and the column names.
func (r *YAMLReader) objectMapSlice(obj yaml.MapSlice) (map[string]any, []string, error) {
	names := make([]string, 0, len(obj))
	row := make(map[string]any)
	for _, item := range obj {
		key := item.Key.(string)
		names = append(names, key)
		row[key] = r.toString(item.Value)
	}
	return row, names, nil
}

// etcRow returns 1 element with column name c1.
func (r *YAMLReader) etcRow(val any) (map[string]any, []string, error) {
	var names []string
	k := "c1"
	names = append(names, k)
	row := make(map[string]any)
	row[k] = r.toString(val)
	return row, names, nil
}

// toString returns a string representation of val.
// It will be YAML if val is a struct or map, otherwise it will be a string representation of val.
func (r *YAMLReader) toString(val any) any {
	var str string
	switch t := val.(type) {
	case nil:
		return nil
	case map[string]any, []yaml.MapSlice, []any:
		b, err := yaml.Marshal(val)
		if err != nil {
			log.Printf("ERROR: YAMLString:%s", err)
		}
		str = yamlToStr(b)
	case []byte:
		str = yamlToStr(t)
	case string:
		str = yamlToStr([]byte(t))
	default:
		str = ValString(t)
	}
	// Remove the last newline.
	str = strings.TrimRight(str, "\n")
	if r.needNULL {
		return replaceNULL(r.inNULL, str)
	}
	return str
}

// yamlToStr converts marshalled YAML to string.
// Values that can be converted to JSON should be JSON.
func yamlToStr(buf []byte) string {
	if !bytes.Contains(buf, []byte("\n")) {
		return ValString(buf)
	}

	// Convert to JSON if it's a YAML element.
	j, err := yaml.YAMLToJSON(buf)
	if err != nil {
		return ValString(buf)
	}
	return ValString(j)
}
