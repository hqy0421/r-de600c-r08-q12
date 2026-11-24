package trdsql

import (
	"bytes"
	"path/filepath"
	"testing"
)

const multiDocFile = "multidoc.yaml"

func runYAMLE2E(t *testing.T, query string, opts ...ReadOpt) string {
	t.Helper()
	outStream := new(bytes.Buffer)
	allOpts := append([]ReadOpt{InFormat(YAML)}, opts...)
	importer := NewImporter(allOpts...)
	exporter := NewExporter(NewWriter(OutStream(outStream)))
	trd := NewTRDSQL(importer, exporter)
	if err := trd.Exec(query); err != nil {
		t.Fatalf("Exec() error: %s", err)
	}
	return outStream.String()
}

func TestTRDSQL_YAMLMultiDocumentReconciliation(t *testing.T) {
	// Three documents (the middle one empty) reconcile to three rows.
	got := runYAMLE2E(t,
		"SELECT count(*) FROM "+filepath.Join(dataDir, multiDocFile),
	)
	if got != "3\n" {
		t.Errorf("count = %q, want 3 (empty document must reconcile)", got)
	}
}

func TestTRDSQL_YAMLDocumentColumn(t *testing.T) {
	got := runYAMLE2E(t,
		"SELECT doc, id, name FROM "+filepath.Join(dataDir, multiDocFile)+" ORDER BY doc",
		InYAMLDocument(true),
	)
	want := "1,1,Orange\n2,,\n3,2,Melon\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

func TestTRDSQL_YAMLDeclaredColumnsExtraNull(t *testing.T) {
	got := runYAMLE2E(t,
		"SELECT id, name FROM "+filepath.Join(dataDir, multiDocFile),
		InYAMLColumns("id", "name"),
		InYAMLExtraMode(YAMLExtraNull),
	)
	want := "1,Orange\n,\n2,Melon\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

func TestTRDSQL_YAMLDeclaredColumnsExtraJSON(t *testing.T) {
	got := runYAMLE2E(t,
		"SELECT id, name, extra FROM "+filepath.Join(dataDir, multiDocFile),
		InYAMLColumns("id", "name"),
		InYAMLExtraMode(YAMLExtraJSON),
	)
	want := "1,Orange,\n,,\n2,Melon,\"{\"\"price\"\":\"\"500\"\"}\"\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}

func TestTRDSQL_YAMLDeclaredColumnsExtraError(t *testing.T) {
	outStream := new(bytes.Buffer)
	importer := NewImporter(
		InFormat(YAML),
		InYAMLColumns("id", "name"),
	)
	exporter := NewExporter(NewWriter(OutStream(outStream)))
	trd := NewTRDSQL(importer, exporter)
	if err := trd.Exec("SELECT id FROM " + filepath.Join(dataDir, multiDocFile)); err == nil {
		t.Error("expected error for undeclared field, got nil")
	}
}

func TestTRDSQL_YAMLDocumentColumnAggregation(t *testing.T) {
	// Reconciliation query: how many rows (including the empty document)
	// each document contributes.
	got := runYAMLE2E(t,
		"SELECT doc, count(*) FROM "+filepath.Join(dataDir, multiDocFile)+" GROUP BY doc ORDER BY doc",
		InYAMLDocument(true),
	)
	want := "1,1\n2,1\n3,1\n"
	if got != want {
		t.Errorf("result = %q, want %q", got, want)
	}
}
