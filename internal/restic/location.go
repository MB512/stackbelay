package restic

import (
	"errors"
	"net/url"
	"strings"
)

// ErrAmbiguousLocation is returned for URL locations that cannot be parsed
// reliably and contain an "@", so they might carry a password that would
// otherwise end up in plain text.
var ErrAmbiguousLocation = errors.New("the repository URL cannot be parsed; if the password contains special " +
	"characters (/ ? # @ :), percent-encode them or enter the password separately")

// URLCredentials inspects a URL-style repository location such as
// "rest:https://alice:secret@host:8000/web". It returns the location
// without user info, and the user and password if the URL contained a
// password (hasPassword). Locations without "://" (local paths,
// "sftp:user@host:/path", ...) are returned unchanged.
func URLCredentials(location string) (clean, user, password string, hasPassword bool, err error) {
	i := strings.Index(location, "://")
	if i < 0 {
		return location, "", "", false, nil
	}
	// The backend prefix ("rest:", "s3:") ends at the last ':' before "://".
	start := strings.LastIndex(location[:i], ":") + 1
	u, err := url.Parse(location[start:])
	if err != nil || u.Host == "" {
		if strings.Contains(location[i:], "@") {
			return "", "", "", false, ErrAmbiguousLocation
		}
		return location, "", "", false, nil
	}
	if u.User == nil {
		// An "@" outside the user info, e.g. a password containing "/"
		// ("https://alice:12/x@host/repo" parses as host "alice:12").
		if strings.Contains(location[i:], "@") {
			return "", "", "", false, ErrAmbiguousLocation
		}
		return location, "", "", false, nil
	}
	password, hasPassword = u.User.Password()
	if !hasPassword {
		return location, "", "", false, nil // only a user name: not secret
	}
	user = u.User.Username()
	u.User = nil
	return location[:start] + u.String(), user, password, true, nil
}

// SplitRESTCredentials moves a password embedded in a REST server URL into
// environment variables, so the location can be stored in plain text:
//
//	rest:https://alice:secret@host:8000/web
//	-> location "rest:https://host:8000/web",
//	   RESTIC_REST_USERNAME=alice, RESTIC_REST_PASSWORD=secret
//
// env is nil if the location carried no password. Other backends do not
// accept credentials in the URL and yield an error.
func SplitRESTCredentials(location string) (clean string, env map[string]string, err error) {
	clean, user, password, has, err := URLCredentials(location)
	if err != nil {
		return "", nil, err
	}
	if !has {
		return clean, nil, nil
	}
	if !strings.HasPrefix(location, "rest:") {
		return "", nil, errors.New("only rest: repository URLs may contain a password; " +
			"enter the backend credentials as variables instead")
	}
	return clean, map[string]string{"RESTIC_REST_USERNAME": user, "RESTIC_REST_PASSWORD": password}, nil
}
