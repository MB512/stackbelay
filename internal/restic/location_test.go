package restic

import (
	"errors"
	"reflect"
	"testing"
)

func TestSplitRESTCredentials(t *testing.T) {
	tests := []struct {
		name      string
		location  string
		wantClean string
		wantEnv   map[string]string
		wantErr   bool
	}{
		{"local path", "/srv/restic/web", "/srv/restic/web", nil, false},
		{"sftp", "sftp:backup@host:/srv/web", "sftp:backup@host:/srv/web", nil, false},
		{"rest without credentials", "rest:https://host:8000/web/", "rest:https://host:8000/web/", nil, false},
		{"rest with user only", "rest:https://alice@host/web", "rest:https://alice@host/web", nil, false},
		{"rest with password", "rest:https://alice:s3cret@host:8000/web",
			"rest:https://host:8000/web", map[string]string{"RESTIC_REST_USERNAME": "alice", "RESTIC_REST_PASSWORD": "s3cret"}, false},
		{"percent-encoded password", "rest:http://alice:a%2Fb%40c%3Ad@10.0.0.5:8000/web",
			"rest:http://10.0.0.5:8000/web", map[string]string{"RESTIC_REST_USERNAME": "alice", "RESTIC_REST_PASSWORD": "a/b@c:d"}, false},
		{"empty password", "rest:https://alice:@host/web",
			"rest:https://host/web", map[string]string{"RESTIC_REST_USERNAME": "alice", "RESTIC_REST_PASSWORD": ""}, false},
		{"s3 with password", "s3:https://key:secret@s3.example/bucket", "", nil, true},
		{"unescaped slash in password", "rest:https://alice:12/x@host/web", "", nil, true},
		{"unescaped hash in password", "rest:https://alice:p#w@host/web", "", nil, true},
		{"unescaped question mark", "rest:https://alice:p?w@host/web", "", nil, true},
		{"s3 plain", "s3:s3.amazonaws.com/bucket", "s3:s3.amazonaws.com/bucket", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clean, env, err := SplitRESTCredentials(tc.location)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q %v", clean, env)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if clean != tc.wantClean || !reflect.DeepEqual(env, tc.wantEnv) {
				t.Fatalf("got %q %v, want %q %v", clean, env, tc.wantClean, tc.wantEnv)
			}
		})
	}
}

func TestURLCredentialsAmbiguous(t *testing.T) {
	_, _, _, _, err := URLCredentials("rest:https://alice:12/x@host/web")
	if !errors.Is(err, ErrAmbiguousLocation) {
		t.Fatalf("want ErrAmbiguousLocation, got %v", err)
	}
}
