package authoring

import "testing"

func TestSafeRel(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "simple note", in: "notes/hello.md", want: "notes/hello.md"},
		{name: "nested", in: "milestones/m1-edge.md", want: "milestones/m1-edge.md"},
		{name: "top-level file", in: "index.md", want: "index.md"},
		{name: "redundant dot segment cleaned", in: "notes/./a.md", want: "notes/a.md"},
		{name: "internal parent that stays inside", in: "notes/sub/../a.md", want: "notes/a.md"},

		{name: "empty", in: "", wantErr: true},
		{name: "just a dir", in: "notes/", wantErr: true},
		{name: "dot", in: ".", wantErr: true},
		{name: "not markdown", in: "notes/a.txt", wantErr: true},
		{name: "no extension", in: "notes/a", wantErr: true},
		{name: "absolute path", in: "/etc/passwd.md", wantErr: true},
		{name: "climbs out with dotdot", in: "../secrets.md", wantErr: true},
		{name: "climbs out then back", in: "../../learnings/x.md", wantErr: true},
		{name: "deep climb", in: "a/../../b.md", wantErr: true},
		{name: "backslash", in: `notes\a.md`, wantErr: true},
		{name: "null byte", in: "notes/a\x00.md", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SafeRel(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SafeRel(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SafeRel(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("SafeRel(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
