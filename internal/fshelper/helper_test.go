package fshelper

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func callHelper(t *testing.T, request Request) Response {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	var output bytes.Buffer
	if err := Serve(bytes.NewReader(encoded), &output); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestServeReadWriteAndEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")
	response := callHelper(t, Request{Op: "write", Path: path, Content: "hello", CreateParents: true})
	if !response.OK {
		t.Fatalf("write failed: %+v", response)
	}
	response = callHelper(t, Request{Op: "edit", Path: path, OldText: "hello", NewText: "world"})
	if !response.OK {
		t.Fatalf("edit failed: %+v", response)
	}
	response = callHelper(t, Request{Op: "read", Path: path, MaxBytes: 3})
	if !response.OK || response.Content != "wor" || !response.Truncated {
		t.Fatalf("unexpected read response: %+v", response)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "world" {
		t.Fatalf("file content = %q, err = %v", content, err)
	}
}

func TestServeEditPreservesFuzzyReplaceError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("same\nsame"), 0644); err != nil {
		t.Fatal(err)
	}
	response := callHelper(t, Request{Op: "edit", Path: path, OldText: "same", NewText: "x"})
	if response.OK || !strings.Contains(response.Error, "匹配到了 2 处") {
		t.Fatalf("error text was not preserved: %+v", response)
	}
}
