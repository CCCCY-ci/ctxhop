package adapter

import (
	"reflect"
	"testing"
)

func TestTouchedFiles(t *testing.T) {
	const root = `D:\Work\Example`

	tool := func(name, path string) string {
		return `{"message":{"content":[{"type":"tool_use","name":"` + name +
			`","input":{"file_path":"` + path + `"}}]}}`
	}

	tests := []struct {
		name    string
		records []string
		want    []FileAccess
	}{
		{
			name:    "a read tool",
			records: []string{tool("Read", `D:\\Work\\Example\\src\\a.go`)},
			want:    []FileAccess{{Path: "src/a.go"}},
		},
		{
			name:    "a write tool",
			records: []string{tool("Edit", `D:\\Work\\Example\\src\\a.go`)},
			want:    []FileAccess{{Path: "src/a.go", Written: true}},
		},
		{
			name: "a file both read and written counts as written",
			records: []string{
				tool("Read", `D:\\Work\\Example\\src\\a.go`),
				tool("Edit", `D:\\Work\\Example\\src\\a.go`),
			},
			want: []FileAccess{{Path: "src/a.go", Written: true}},
		},
		{
			name: "order does not change the verdict",
			records: []string{
				tool("Edit", `D:\\Work\\Example\\src\\a.go`),
				tool("Read", `D:\\Work\\Example\\src\\a.go`),
			},
			want: []FileAccess{{Path: "src/a.go", Written: true}},
		},
		{
			name:    "an unknown tool is treated as a read",
			records: []string{tool("SomeNewTool", `D:\\Work\\Example\\src\\a.go`)},
			// Over-reporting a read costs one hash comparison; missing a write
			// would let a stale file through unnoticed.
			want: []FileAccess{{Path: "src/a.go"}},
		},
		{
			name:    "files outside the project are dropped",
			records: []string{tool("Edit", `C:\\Users\\alice\\.claude\\skills\\x.md`)},
			want:    []FileAccess{},
		},
		{
			name:    "a sibling directory sharing the prefix is outside",
			records: []string{tool("Edit", `D:\\Work\\Example2\\a.go`)},
			want:    []FileAccess{},
		},
		{
			name:    "the project root itself is not a file",
			records: []string{tool("Read", `D:\\Work\\Example`)},
			want:    []FileAccess{},
		},
		{
			name:    "forward slashes and case differences still match",
			records: []string{tool("Edit", `d:/work/example/src/b.go`)},
			want:    []FileAccess{{Path: "src/b.go", Written: true}},
		},
		{
			name: "results are sorted",
			records: []string{
				tool("Read", `D:\\Work\\Example\\z.go`),
				tool("Read", `D:\\Work\\Example\\a.go`),
			},
			want: []FileAccess{{Path: "a.go"}, {Path: "z.go"}},
		},
		{
			name: "a notebook edit is not missed",
			// NotebookEdit names its argument notebook_path, not file_path.
			// Reading only file_path produced no entry at all - not even a
			// read - so the consistency check would call the workspace clean
			// and the user would resume against a stale notebook.
			records: []string{
				`{"message":{"content":[{"type":"tool_use","name":"NotebookEdit","input":{"notebook_path":"D:\\Work\\Example\\analysis.ipynb"}}]}}`,
			},
			want: []FileAccess{{Path: "analysis.ipynb", Written: true}},
		},
		{
			name: "shell commands contribute nothing",
			// The gap PoC-2 measured: no file path is recorded, so the
			// consistency check cannot rely on this set alone.
			records: []string{
				`{"message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go build ./..."}}]}}`,
			},
			want: []FileAccess{},
		},
		{
			name: "non-tool blocks and odd shapes are ignored",
			records: []string{
				`{"message":{"content":[{"type":"text","text":"hello"}]}}`,
				`{"message":{"content":"not a list"}}`,
				`{"message":"not an object"}`,
				`{"type":"system"}`,
				`{"message":{"content":[{"type":"tool_use","name":"Edit"}]}}`,
				`{"message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":""}}]}}`,
			},
			want: []FileAccess{},
		},
		{
			name: "an unparseable record does not hide the rest",
			records: []string{
				`{"message":{"content":`,
				tool("Edit", `D:\\Work\\Example\\src\\a.go`),
			},
			want: []FileAccess{{Path: "src/a.go", Written: true}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records := make([][]byte, len(tt.records))
			for i, r := range tt.records {
				records[i] = []byte(r)
			}
			got := TouchedFiles(records, root)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestTouchedFilesWithoutAProjectRoot(t *testing.T) {
	records := [][]byte{[]byte(`{"message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"D:\\a.go"}}]}}`)}
	if got := TouchedFiles(records, ""); len(got) != 0 {
		t.Errorf("got %+v, want nothing without a project root", got)
	}
}
