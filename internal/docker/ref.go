package docker

import "strings"

// NormalizeRef brings an image reference into the canonical form Docker
// uses internally, so that different spellings of the same image compare
// equal (issue #40):
//
//	nginx                         -> docker.io/library/nginx:latest
//	zabbix/zabbix-agent           -> docker.io/zabbix/zabbix-agent:latest
//	index.docker.io/library/nginx -> docker.io/library/nginx:latest
//	host:5000/app                 -> host:5000/app:latest
//	ghcr.io/owner/app@sha256:...  -> ghcr.io/owner/app@sha256:... (no tag added)
//
// The first path component is a registry only if it contains "." or ":"
// or is "localhost"; only the registry is lower-cased (the path is lower
// case by spec). It is the normalization of distribution/reference
// (ParseNormalizedNamed + TagNameOnly) without the dependency. It does not
// validate: use it for comparisons only and show references as written.
// An empty reference stays empty.
func NormalizeRef(ref string) string {
	if ref == "" {
		return ""
	}
	name, digest, hasDigest := strings.Cut(ref, "@")
	tag := ""
	// The tag is after the last ":" behind the last "/": "host:5000/app"
	// has none.
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name, tag = name[:i], name[i+1:]
	}
	domain, path := "docker.io", name
	if first, rest, ok := strings.Cut(name, "/"); ok &&
		(strings.ContainsAny(first, ".:") || first == "localhost") {
		domain, path = strings.ToLower(first), rest
	}
	if domain == "index.docker.io" {
		domain = "docker.io"
	}
	if domain == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	if tag == "" && !hasDigest {
		tag = "latest"
	}
	out := domain + "/" + path
	if tag != "" {
		out += ":" + tag
	}
	if hasDigest {
		out += "@" + digest
	}
	return out
}

// SameRef reports whether a and b name the same image reference in
// different spellings (see NormalizeRef).
func SameRef(a, b string) bool {
	return NormalizeRef(a) == NormalizeRef(b)
}

// RefRepository returns the normalized repository of ref, without tag and
// digest: "nginx:1.27" and "docker.io/library/nginx@sha256:..." both give
// "docker.io/library/nginx".
func RefRepository(ref string) string {
	return repository(NormalizeRef(ref))
}
