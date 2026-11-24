package web

import (
	"reflect"
	"testing"

	"github.com/MB512/stackbelay/internal/store"
)

func TestRepoType(t *testing.T) {
	tests := []struct {
		location string
		env      map[string]string
		want     string
	}{
		{"rest:https://h/r", nil, repoTypeREST},
		{"rest:https://h/r", map[string]string{envRESTUser: "u", envRESTPass: "p"}, repoTypeREST},
		{"s3:s3.example/b", map[string]string{envS3KeyID: "k", envS3Secret: "s", envS3Region: "r"}, repoTypeS3},
		// A variable the type has no field for: Other, so nothing is lost.
		{"s3:s3.example/b", map[string]string{envS3KeyID: "k", "AWS_SESSION_TOKEN": "t"}, repoTypeOther},
		{"rest:https://h/r", map[string]string{"B2_ACCOUNT_ID": "x"}, repoTypeOther},
		{"b2:bucket:path", nil, repoTypeOther},
		{"/srv/restic", nil, repoTypeOther},
	}
	for _, tt := range tests {
		if got := repoType(tt.location, tt.env); got != tt.want {
			t.Errorf("repoType(%q, %v) = %q, want %q", tt.location, tt.env, got, tt.want)
		}
	}
}

// The form never carries a secret value, only that one is stored.
func TestNewRepoFormMasksSecrets(t *testing.T) {
	f := newRepoForm(&store.Repository{ID: 3, Name: "nas", Location: "s3:s3.example/b", Password: "repo-pw",
		Env: map[string]string{envS3KeyID: "AK", envS3Secret: "sk", envS3Region: "eu"}})
	if f.Type != repoTypeS3 || f.S3Location != "s3.example/b" || f.S3KeyID != "AK" || f.S3Region != "eu" ||
		!f.S3SecretStored || !f.PasswordStored {
		t.Fatalf("form %+v", f)
	}
	want := []envRow{{Key: envS3KeyID, Value: "AK"}, {Key: envS3Region, Value: "eu"}, {Key: envS3Secret, Secret: true, Stored: true}}
	if !reflect.DeepEqual(f.Rows, want) {
		t.Fatalf("rows %+v", f.Rows)
	}
	if empty := newRepoForm(nil); empty.Type != repoTypeREST || empty.PasswordStored {
		t.Fatalf("new form %+v", empty)
	}
}
