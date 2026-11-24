package web

import (
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/store"
)

// The repository form (issue #45): one section per backend type, the
// values of non-secret variables shown, secret ones never sent to the
// browser. The type is not stored: it is derived from the location prefix
// and the variables (repoType), and each type's fields map to the location
// and the matching variables.

// Repository types of the form.
const (
	repoTypeREST  = "rest"
	repoTypeS3    = "s3"
	repoTypeOther = "other"
)

// Variables of the REST and S3 types.
const (
	envRESTUser    = "RESTIC_REST_USERNAME"
	envRESTPass    = "RESTIC_REST_PASSWORD"
	envS3KeyID     = "AWS_ACCESS_KEY_ID"
	envS3Secret    = "AWS_SECRET_ACCESS_KEY" //nolint:gosec // G101: the name of the variable, not a credential
	envS3Region    = "AWS_DEFAULT_REGION"
	emptyEnvRows   = 2 // empty rows of the variables table (without JavaScript)
	maxEnvFormRows = 32
)

// repoForm is the add/edit form of a repository. It never holds secret
// values, only whether one is stored.
type repoForm struct {
	ID             int64 // 0 = new repository
	Name           string
	Type           string // repoTypeREST, repoTypeS3 or repoTypeOther
	PasswordStored bool   // the repository password (always set for a saved repository)

	// REST server: location "rest:" + RestURL.
	RestURL, RestUser  string
	RestPasswordStored bool

	// S3: location "s3:" + S3Location.
	S3Location, S3KeyID, S3Region string
	S3SecretStored                bool

	// Other: the restic repository string and a table of variables.
	Location string
	Rows     []envRow
	Keys     []envKey // the keys a row may have

	// OfferInit shows the "Initialize repository" button enabled.
	OfferInit bool
}

// envRow is one row of the variables table.
type envRow struct {
	Key    string
	Value  string // "" for a secret
	Secret bool
	Stored bool // a secret value is stored (shown masked)
}

// envRowData is the data of the "env-row" partial.
type envRowData struct {
	Row  envRow
	Keys []envKey
}

// envKey is one choice of the key drop-down.
type envKey struct {
	Name   string
	Secret bool
}

// allowedRepoEnv are the backend variables the form accepts. The
// repository password has its own field.
func allowedRepoEnv() []string {
	var out []string
	for _, k := range restic.BackendEnvVars {
		if k != "RESTIC_PASSWORD" && k != "RESTIC_PASSWORD_FILE" {
			out = append(out, k)
		}
	}
	return out
}

// repoType derives the form type of a stored repository: REST or S3 when
// the location has that prefix and only that type's variables are set,
// otherwise Other.
func repoType(location string, env map[string]string) string {
	only := func(keys ...string) bool {
		for k := range env {
			if !slices.Contains(keys, k) {
				return false
			}
		}
		return true
	}
	switch {
	case strings.HasPrefix(location, "rest:") && only(envRESTUser, envRESTPass):
		return repoTypeREST
	case strings.HasPrefix(location, "s3:") && only(envS3KeyID, envS3Secret, envS3Region):
		return repoTypeS3
	}
	return repoTypeOther
}

// newRepoForm returns the form for a stored repository, or an empty one
// (type REST) for nil. All sections are filled where the stored data fits,
// so switching the type in the browser keeps what can be kept.
func newRepoForm(repo *store.Repository) repoForm {
	if repo == nil {
		return repoForm{Type: repoTypeREST}
	}
	f := repoForm{ID: repo.ID, Name: repo.Name, Type: repoType(repo.Location, repo.Env),
		PasswordStored: repo.Password != "", Location: repo.Location}
	if url, ok := strings.CutPrefix(repo.Location, "rest:"); ok {
		f.RestURL = url
	}
	if loc, ok := strings.CutPrefix(repo.Location, "s3:"); ok {
		f.S3Location = loc
	}
	f.RestUser, f.RestPasswordStored = repo.Env[envRESTUser], repo.Env[envRESTPass] != ""
	f.S3KeyID, f.S3Region, f.S3SecretStored = repo.Env[envS3KeyID], repo.Env[envS3Region], repo.Env[envS3Secret] != ""
	for _, k := range slices.Sorted(maps.Keys(repo.Env)) {
		f.Rows = append(f.Rows, storedRow(k, repo.Env[k]))
	}
	return f
}

// storedRow is the table row of a stored variable: the value only if it
// is not a secret.
func storedRow(key, value string) envRow {
	if restic.IsSecretEnvVar(key) {
		return envRow{Key: key, Secret: true, Stored: value != ""}
	}
	return envRow{Key: key, Value: value}
}

// prepare fills what every rendering of the form needs: the key choices
// and the empty rows of the variables table.
func (f *repoForm) prepare() {
	if f.Type == "" {
		f.Type = repoTypeREST
	}
	f.Keys = f.Keys[:0]
	for _, k := range allowedRepoEnv() {
		f.Keys = append(f.Keys, envKey{Name: k, Secret: restic.IsSecretEnvVar(k)})
	}
	for range emptyEnvRows {
		f.Rows = append(f.Rows, envRow{})
	}
}

// repoInput is a submitted repository form, validated.
type repoInput struct {
	name, location, password string
	env                      map[string]string
	fromURL                  bool // credentials were moved from the URL into env
}

// parseRepoInput reads and validates the repository form: name, type, the
// fields of that type and the repository password. Empty secret fields
// keep the value stored in old (nil for a new repository). A user name
// and password in a rest: URL are moved into the variables (the location
// is stored in plain text). On error it returns a message for the user
// and the form to show again (never a secret value; the location only
// once it is known to contain no password).
func parseRepoInput(r *http.Request, old *store.Repository) (repoInput, repoForm, string) {
	echo := newRepoForm(old)
	in := repoInput{name: strings.TrimSpace(r.PostFormValue("name")), env: map[string]string{}}
	echo.Name = in.name
	oldEnv := map[string]string{}
	if old != nil {
		oldEnv = old.Env
	}
	// set stores a variable; an empty secret keeps the stored value, an
	// empty value of any other variable leaves it unset.
	set := func(key, value string) {
		if value == "" && restic.IsSecretEnvVar(key) {
			value = oldEnv[key]
		}
		if value != "" {
			in.env[key] = value
		}
	}
	fail := func(err error) (repoInput, repoForm, string) {
		return in, echo, capitalize(err.Error()) + "."
	}

	echo.Type = r.PostFormValue("type")
	var urlEnv map[string]string
	switch echo.Type {
	case repoTypeREST:
		raw := strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("rest_url")), "rest:")
		echo.RestURL, echo.RestUser = "", strings.TrimSpace(r.PostFormValue("rest_user"))
		loc, ue, err := restic.SplitRESTCredentials("rest:" + raw)
		if err != nil {
			return fail(err) // the URL may contain a password: not echoed
		}
		in.location, urlEnv, echo.RestURL = loc, ue, strings.TrimPrefix(loc, "rest:")
		set(envRESTUser, echo.RestUser)
		set(envRESTPass, r.PostFormValue("rest_password"))
	case repoTypeS3:
		raw := strings.TrimSpace(r.PostFormValue("s3_location"))
		echo.S3Location = ""
		echo.S3KeyID = strings.TrimSpace(r.PostFormValue("s3_key_id"))
		echo.S3Region = strings.TrimSpace(r.PostFormValue("s3_region"))
		if raw != "" && !strings.HasPrefix(raw, "s3:") {
			raw = "s3:" + raw
		}
		loc, _, err := restic.SplitRESTCredentials(raw) // refuses credentials in the URL
		if err != nil {
			return fail(err)
		}
		in.location, echo.S3Location = loc, strings.TrimPrefix(loc, "s3:")
		set(envS3KeyID, echo.S3KeyID)
		set(envS3Secret, r.PostFormValue("s3_secret"))
		set(envS3Region, echo.S3Region)
	case repoTypeOther:
		echo.Location = ""
		loc, ue, err := restic.SplitRESTCredentials(strings.TrimSpace(r.PostFormValue("location")))
		if err != nil {
			return fail(err)
		}
		in.location, urlEnv, echo.Location = loc, ue, loc
		keys, values := r.PostForm["env_key"], r.PostForm["env_value"]
		if len(keys) != len(values) || len(keys) > maxEnvFormRows {
			return in, echo, "The variables table is incomplete. Reload the page and try again."
		}
		echo.Rows = nil
		for i, key := range keys {
			if key == "" {
				continue
			}
			if !slices.Contains(allowedRepoEnv(), key) {
				// Only the key is echoed, never the value.
				return in, echo, "Variable " + truncate(key, 40) + " is not supported."
			}
			if _, dup := in.env[key]; dup || slices.ContainsFunc(echo.Rows, func(e envRow) bool { return e.Key == key }) {
				return in, echo, key + " is set twice."
			}
			set(key, values[i])
			row := storedRow(key, in.env[key])
			if !row.Secret {
				row.Value = values[i]
			}
			echo.Rows = append(echo.Rows, row)
		}
	default:
		return in, echo, "Choose the type of the repository."
	}
	maps.Copy(in.env, urlEnv) // credentials from the URL win
	in.fromURL = len(urlEnv) > 0

	if err := store.ValidateRepositoryName(in.name); err != nil {
		return fail(err)
	}
	in.password = r.PostFormValue("password")
	if in.password == "" && old != nil {
		in.password = old.Password
	}
	if err := store.ValidateRepository(in.location, in.password, in.env); err != nil {
		return fail(err)
	}
	return in, echo, ""
}

// savedInfo is the message after saving a repository.
func (in repoInput) savedInfo() string {
	info := "Repository saved."
	if in.fromURL {
		info += " The user name and password from the URL are stored encrypted as variables, not in the location."
	}
	return info
}
