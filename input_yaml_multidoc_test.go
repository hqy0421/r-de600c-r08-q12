package trdsql

import (
	"io"
	"reflect"
	"strings"
	"testing"
)

const multiDocYAML = "id: 1\nname: Orange\n---\n---\nid: 2\nname: Melon\nprice: \"500\"\n"

func TestYAMLReader_MultiDocument(t *testing.T) {
	r, err := NewYAMLReader(strings.NewReader(multiDocYAML), NewReadOpts(InPreRead(10)))
	if err != nil {
		t.Fatal(err)
	}

	gotNames, _ := r.Names()
	if !arraySortEqual(t, gotNames, []string{"id", "name", "price"}) {
		t.Errorf("Names() = %v", gotNames)
	}

	// Three documents (the middle one empty) reconcile to three rows.
	rows := r.PreReadRow()
	if len(rows) != 3 {
		t.Fatalf("PreReadRow() len = %d, want 3 (empty document must reconcile)", len(rows))
	}
	if !reflect.DeepEqual(rows[1], []any{nil, nil, nil}) {
		t.Errorf("empty document row = %#v, want all NULL", rows[1])
	}
	// The later document's extra field is captured in inferred mode.
	wantLast := map[string]any{"id": "2", "name": "Melon", "price": "500"}
	gotLast := map[string]any{}
	for i, name := range gotNames {
		gotLast[name] = rows[2][i]
	}
	if !reflect.DeepEqual(gotLast, wantLast) {
		t.Errorf("last row = %#v, want %#v", gotLast, wantLast)
	}

	if _, err := r.ReadRow(make([]any, len(gotNames))); err != io.EOF {
		t.Errorf("ReadRow() after pre-read err = %v, want EOF", err)
	}
}

func TestYAMLReader_DocumentColumn(t *testing.T) {
	r, err := NewYAMLReader(
		strings.NewReader(multiDocYAML),
		NewReadOpts(InYAMLDocument(true), InPreRead(10)),
	)
	if err != nil {
		t.Fatal(err)
	}

	names, _ := r.Names()
	if names[0] != "doc" {
		t.Fatalf("first column = %q, want doc (names=%v)", names[0], names)
	}
	wantSet := map[string]bool{"doc": true, "id": true, "name": true, "price": true}
	gotSet := map[string]bool{}
	for _, n := range names {
		gotSet[n] = true
	}
	if !reflect.DeepEqual(gotSet, wantSet) {
		t.Fatalf("Names() = %v, want set %v", names, wantSet)
	}
	types, _ := r.Types()
	if types[0] != "int" {
		t.Errorf("doc column type = %s, want int", types[0])
	}

	rows := r.PreReadRow()
	wantDocs := []int{1, 2, 3}
	for i, row := range rows {
		if row[0] != wantDocs[i] {
			t.Errorf("row %d doc = %#v, want %d", i, row[0], wantDocs[i])
		}
	}
	// The empty document keeps its document identity with NULL data.
	wantEmpty := map[string]any{"doc": 2, "id": nil, "name": nil, "price": nil}
	gotEmpty := map[string]any{}
	for i, n := range names {
		gotEmpty[n] = rows[1][i]
	}
	if !reflect.DeepEqual(gotEmpty, wantEmpty) {
		t.Errorf("empty document row = %#v, want %#v", gotEmpty, wantEmpty)
	}
}

func TestYAMLReader_DeclaredColumnsExtraError(t *testing.T) {
	_, err := NewYAMLReader(
		strings.NewReader(multiDocYAML),
		NewReadOpts(InYAMLColumns("id", "name")),
	)
	if err == nil {
		t.Fatal("expected error for undeclared field price, got nil")
	}
}

func TestYAMLReader_DeclaredColumnsExtraNull(t *testing.T) {
	r, err := NewYAMLReader(
		strings.NewReader(multiDocYAML),
		NewReadOpts(InPreRead(10), InYAMLColumns("id", "name"), InYAMLExtraMode(YAMLExtraNull)),
	)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := r.Names()
	if !reflect.DeepEqual(names, []string{"id", "name"}) {
		t.Fatalf("Names() = %v", names)
	}
	rows := r.PreReadRow()
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if !reflect.DeepEqual(rows[2], []any{"2", "Melon"}) {
		t.Errorf("last row = %#v, price must be discarded", rows[2])
	}
	if !reflect.DeepEqual(rows[1], []any{nil, nil}) {
		t.Errorf("empty row = %#v", rows[1])
	}
}

func TestYAMLReader_DeclaredColumnsExtraJSON(t *testing.T) {
	r, err := NewYAMLReader(
		strings.NewReader(multiDocYAML),
		NewReadOpts(InPreRead(10), InYAMLColumns("id", "name"), InYAMLExtraMode(YAMLExtraJSON)),
	)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := r.Names()
	if !reflect.DeepEqual(names, []string{"id", "name", "extra"}) {
		t.Fatalf("Names() = %v", names)
	}
	rows := r.PreReadRow()
	// Rows without undeclared fields get NULL in the extra column.
	if rows[0][2] != nil {
		t.Errorf("first extra = %#v, want nil", rows[0][2])
	}
	if rows[1][2] != nil {
		t.Errorf("empty doc extra = %#v, want nil", rows[1][2])
	}
	if rows[2][2] != `{"price":"500"}` {
		t.Errorf("last extra = %#v, want {\"price\":\"500\"}", rows[2][2])
	}
}

func TestYAMLReader_DeclaredColumnsMissingFields(t *testing.T) {
	const src = "id: 1\nname: Orange\n---\nid: 2\n"
	r, err := NewYAMLReader(
		strings.NewReader(src),
		NewReadOpts(InPreRead(10), InYAMLColumns("id", "name"), InYAMLExtraMode(YAMLExtraNull)),
	)
	if err != nil {
		t.Fatal(err)
	}
	rows := r.PreReadRow()
	// The second document lacks the declared "name" field: import NULL.
	if !reflect.DeepEqual(rows[1], []any{"2", nil}) {
		t.Errorf("missing-field row = %#v, want [2 nil]", rows[1])
	}
}

func TestYAMLReader_EmptyStream(t *testing.T) {
	r, err := NewYAMLReader(strings.NewReader(""), NewReadOpts(InYAMLDocument(true)))
	if err != nil {
		t.Fatal(err)
	}
	names, _ := r.Names()
	if !reflect.DeepEqual(names, []string{"doc"}) {
		t.Errorf("Names() = %v, want [doc]", names)
	}
	if rows := r.PreReadRow(); len(rows) != 0 {
		t.Errorf("PreReadRow() = %v, want no rows", rows)
	}
	if _, err := r.ReadRow(nil); err != io.EOF {
		t.Errorf("ReadRow() err = %v, want EOF", err)
	}
}

func TestYAMLReader_OnlyEmptyDocuments(t *testing.T) {
	r, err := NewYAMLReader(
		strings.NewReader("---\n---\n"),
		NewReadOpts(InPreRead(10), InYAMLDocument(true)),
	)
	if err != nil {
		t.Fatal(err)
	}
	rows := r.PreReadRow()
	if !reflect.DeepEqual(rows, [][]any{{1}, {2}}) {
		t.Errorf("rows = %#v, want two identity-only reconciliation rows", rows)
	}
}

func TestYAMLReader_DocNameCollision(t *testing.T) {
	const src = "doc: x\nv: 1\n---\nv: 2\n"
	r, err := NewYAMLReader(
		strings.NewReader(src),
		NewReadOpts(InPreRead(10), InYAMLDocument(true)),
	)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := r.Names()
	// The data column keeps "doc"; the identity column is renamed to
	// "doc1" regardless of map iteration order.
	wantSet := map[string]bool{"doc1": true, "doc": true, "v": true}
	gotSet := map[string]bool{}
	for _, n := range names {
		gotSet[n] = true
	}
	if !reflect.DeepEqual(gotSet, wantSet) {
		t.Fatalf("Names() = %v, want set %v", names, wantSet)
	}
	byName := func(row []any) map[string]any {
		m := map[string]any{}
		for i, n := range names {
			m[n] = row[i]
		}
		return m
	}
	rows := r.PreReadRow()
	first, second := byName(rows[0]), byName(rows[1])
	if first["doc1"] != 1 || first["doc"] != "x" || first["v"] != "1" {
		t.Errorf("first row = %#v", first)
	}
	if second["doc1"] != 2 || second["doc"] != nil || second["v"] != "2" {
		t.Errorf("second row = %#v", second)
	}
}

func TestYAMLReader_DocNameCollisionTwice(t *testing.T) {
	r, err := NewYAMLReader(
		strings.NewReader("doc: a\ndoc1: b\nv: 1\n"),
		NewReadOpts(InPreRead(10), InYAMLDocument(true)),
	)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := r.Names()
	wantSet := map[string]bool{"doc2": true, "doc": true, "doc1": true, "v": true}
	gotSet := map[string]bool{}
	for _, n := range names {
		gotSet[n] = true
	}
	if !reflect.DeepEqual(gotSet, wantSet) {
		t.Fatalf("Names() = %v, want set %v", names, wantSet)
	}
	// The identity column must be the first column, before data columns.
	if r.docName != "doc2" || names[0] != "doc2" {
		t.Errorf("identity column = %q (names=%v), want doc2 first", r.docName, names)
	}
}

func TestYAMLReader_ExtraNameCollision(t *testing.T) {
	// "extra" is a declared data column, so the JSON column is renamed.
	const src = "a: 1\nextra: raw\n---\nb: 2\n"
	r, err := NewYAMLReader(
		strings.NewReader(src),
		NewReadOpts(InPreRead(10), InYAMLColumns("a", "extra"), InYAMLExtraMode(YAMLExtraJSON)),
	)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := r.Names()
	wantSet := map[string]bool{"a": true, "extra": true, "extra1": true}
	gotSet := map[string]bool{}
	for _, n := range names {
		gotSet[n] = true
	}
	if !reflect.DeepEqual(gotSet, wantSet) {
		t.Fatalf("Names() = %v, want set %v", names, wantSet)
	}
	rows := r.PreReadRow()
	byName := func(row []any) map[string]any {
		m := map[string]any{}
		for i, n := range names {
			m[n] = row[i]
		}
		return m
	}
	first, second := byName(rows[0]), byName(rows[1])
	// The declared "extra" column keeps the raw value; the JSON payload
	// lands in the renamed "extra1".
	if first["extra"] != "raw" || first["extra1"] != nil {
		t.Errorf("first row = %#v", first)
	}
	if second["extra1"] != `{"b":"2"}` {
		t.Errorf("second row extra1 = %#v", second["extra1"])
	}
}

func TestYAMLReader_DocEndMarker(t *testing.T) {
	r, err := NewYAMLReader(
		strings.NewReader("a: 1\n...\n---\na: 2\n"),
		NewReadOpts(InPreRead(10), InYAMLDocument(true)),
	)
	if err != nil {
		t.Fatal(err)
	}
	rows := r.PreReadRow()
	if !reflect.DeepEqual(rows, [][]any{{1, "1"}, {2, "2"}}) {
		t.Errorf("rows = %#v", rows)
	}
}

func TestYAMLReader_ScalarDocuments(t *testing.T) {
	r, err := NewYAMLReader(
		strings.NewReader("hello\n---\nworld\n"),
		NewReadOpts(InPreRead(10), InYAMLDocument(true)),
	)
	if err != nil {
		t.Fatal(err)
	}
	rows := r.PreReadRow()
	if !reflect.DeepEqual(rows, [][]any{{1, "hello"}, {2, "world"}}) {
		t.Errorf("rows = %#v", rows)
	}
}

func TestYAMLReader_MultibyteOffsets(t *testing.T) {
	const src = "name: 田中\n---\nname: 佐藤\nnote: メモ\n"
	r, err := NewYAMLReader(
		strings.NewReader(src),
		NewReadOpts(InPreRead(10), InYAMLDocument(true)),
	)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := r.Names()
	rows := r.PreReadRow()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 documents", len(rows))
	}
	last := map[string]any{}
	for i, n := range names {
		last[n] = rows[1][i]
	}
	if last["name"] != "佐藤" || last["note"] != "メモ" || last["doc"] != 2 {
		t.Errorf("multibyte last row = %#v", last)
	}
}

func TestYAMLReader_LimitRead(t *testing.T) {
	r, err := NewYAMLReader(
		strings.NewReader(multiDocYAML),
		NewReadOpts(InYAMLDocument(true), InLimitRead(true)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if rows := r.PreReadRow(); len(rows) != 1 {
		t.Errorf("pre-read rows = %d, want 1", len(rows))
	}
	if _, err := r.ReadRow(make([]any, 4)); err != io.EOF {
		t.Errorf("ReadRow() err = %v, want EOF under limit read", err)
	}
}

func TestYAMLSegments(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "single document",
			src:  "a: 1\n",
			want: []string{"a: 1\n"},
		},
		{
			name: "two documents",
			src:  "a: 1\n---\na: 2\n",
			want: []string{"a: 1\n", "---\na: 2\n"},
		},
		{
			name: "empty document preserved",
			src:  "a: 1\n---\n---\na: 2\n",
			want: []string{"a: 1\n", "---\n", "---\na: 2\n"},
		},
		{
			name: "leading header",
			src:  "---\na: 1\n---\na: 2\n",
			want: []string{"---\na: 1\n", "---\na: 2\n"},
		},
		{
			name: "multibyte boundary",
			src:  "name: 田中\n---\na: 1\n",
			want: []string{"name: 田中\n", "---\na: 1\n"},
		},
		{
			name: "comment-only implicit document",
			src:  "# header comment\n---\na: 1\n",
			want: []string{"# header comment", "\n---\na: 1\n"},
		},
		{
			name: "blank prefix is not a document",
			src:  "\n\n---\na: 1\n",
			want: []string{"---\na: 1\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := yamlSegments([]byte(tt.src))
			gotStr := make([]string, len(got))
			for i, seg := range got {
				gotStr[i] = string(seg)
			}
			if !reflect.DeepEqual(gotStr, tt.want) {
				t.Errorf("yamlSegments() = %#v, want %#v", gotStr, tt.want)
			}
		})
	}
}

func TestYAMLReader_CommentOnlyImplicitDocument(t *testing.T) {
	r, err := NewYAMLReader(
		strings.NewReader("# header comment\n---\na: 1\n---\na: 2\n"),
		NewReadOpts(InPreRead(10), InYAMLDocument(true)),
	)
	if err != nil {
		t.Fatal(err)
	}
	rows := r.PreReadRow()
	// The comment-only implicit document reconciles to a NULL row.
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 documents", len(rows))
	}
	if rows[0][0] != 1 || rows[0][1] != nil {
		t.Errorf("comment-only doc row = %#v, want identity 1 with NULL data", rows[0])
	}
	if rows[1][0] != 1+1 || rows[2][0] != 3 {
		t.Errorf("document identity not consecutive: %#v", rows)
	}
}

func TestYAMLReader_JQWithDocuments(t *testing.T) {
	const src = "- id: 1\n  name: Orange\n---\n- id: 2\n  name: Melon\n"
	r, err := NewYAMLReader(
		strings.NewReader(src),
		NewReadOpts(InPreRead(10), InJQ(".[0]"), InYAMLDocument(true)),
	)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := r.Names()
	rows := r.PreReadRow()
	want := []map[string]any{
		{"doc": 1, "id": "1", "name": "Orange"},
		{"doc": 2, "id": "2", "name": "Melon"},
	}
	for i, row := range rows {
		got := map[string]any{}
		for j, n := range names {
			got[n] = row[j]
		}
		if !reflect.DeepEqual(got, want[i]) {
			t.Errorf("row %d = %#v, want %#v", i, got, want[i])
		}
	}
}
