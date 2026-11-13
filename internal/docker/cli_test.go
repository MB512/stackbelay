package docker

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePS(t *testing.T) {
	want := []psEntry{
		{ID: "abc", Service: "app", State: "running", Health: "healthy", Image: "nextcloud:29"},
		{ID: "def", Service: "db", State: "exited", Image: "postgres:16"},
	}
	tests := []struct {
		name string
		in   string
		want []psEntry
	}{
		{"empty", "", nil},
		{"empty array", "[]", []psEntry{}},
		{"array", `[{"ID":"abc","Service":"app","State":"running","Health":"healthy","Image":"nextcloud:29"},{"ID":"def","Service":"db","State":"exited","Health":"","Image":"postgres:16"}]`, want},
		{"ndjson", `{"ID":"abc","Service":"app","State":"running","Health":"healthy","Image":"nextcloud:29","Extra":1}
{"ID":"def","Service":"db","State":"exited","Health":"","Image":"postgres:16"}
`, want},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePS([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	if _, err := parsePS([]byte("{not json")); err == nil {
		t.Fatal("expected error for invalid json")
	}
}

func TestHealthStatus(t *testing.T) {
	cs := []psEntry{
		{Service: "ok", State: "running"},
		{Service: "healthy", State: "running", Health: "healthy"},
		{Service: "starting", State: "running", Health: "starting"},
		{Service: "created", State: "created"},
		{Service: "sick", State: "running", Health: "unhealthy"},
		{Service: "dead", State: "exited"},
		{Service: "scaled", State: "running"},
		{Service: "scaled", State: "restarting"},
	}
	tests := []struct {
		name        string
		services    []string
		wantPending []string
		wantErr     string
	}{
		{"all ready", []string{"ok", "healthy"}, nil, ""},
		{"pending", []string{"ok", "starting", "created", "missing", "scaled"}, []string{"starting", "created", "missing", "scaled"}, ""},
		{"unhealthy fails", []string{"ok", "sick"}, nil, "sick (unhealthy)"},
		{"exited fails", []string{"dead", "starting"}, nil, "dead (exited)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pending, err := healthStatus(cs, tt.services)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(pending, tt.wantPending) {
				t.Fatalf("pending = %v, want %v", pending, tt.wantPending)
			}
		})
	}
}
