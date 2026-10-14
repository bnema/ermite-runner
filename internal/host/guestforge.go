// SPDX-License-Identifier: MIT

package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"bnema.dev/ermite-runner/internal/urlorigin"
)

// serverURLKeys are the keys of the github context that actions use to reach
// the forge: the server's web URL, its REST API and its GraphQL API. The
// environment variables GITHUB_SERVER_URL, GITHUB_API_URL and GITHUB_GRAPHQL_URL
// are derived from them by the executor (State.RuntimeEnv), so rewriting the
// context rewrites the environment too. Everything else that carries the
// origin (the URLs inside github.event, recorded as the server built them;
// repositoryUrl, which is a git:// URL) is left alone.
var serverURLKeys = []string{"server_url", "api_url", "graphql_url"}

type jsonMember struct {
	key string
	val json.RawMessage
}

// RewriteServerURLs returns the contexts JSON of a job with the origin of
// github.server_url, github.api_url and github.graphql_url replaced by target
// (scheme://host[:port], the ERMITE_RUNNER_URL origin) when, and only when, the
// value has the same origin as github.server_url, the server's public origin.
// Paths, queries and fragments are kept ("/-/github/api"); values with another
// origin, with credentials, or that are not strings are untouched, as is the
// rest of the document (order of keys included). The input is returned as it
// is when nothing changes.
func RewriteServerURLs(contexts, target string) (string, error) {
	if target == "" {
		return contexts, nil
	}
	top, err := parseObject([]byte(contexts))
	if err != nil {
		return "", fmt.Errorf("job contexts: %w", err)
	}
	gi := -1
	for i, m := range top {
		if m.key == "github" {
			gi = i
		}
	}
	if gi < 0 {
		return contexts, nil
	}
	gh, err := parseObject(top[gi].val)
	if err != nil {
		return "", fmt.Errorf("job contexts: github: %w", err)
	}
	str := func(raw json.RawMessage) (string, bool) {
		var s string
		return s, json.Unmarshal(raw, &s) == nil
	}
	var public string
	for _, m := range gh {
		if m.key == "server_url" {
			if s, ok := str(m.val); ok {
				public = originOf(s)
			}
		}
	}
	if public == "" || public == originOf(target) {
		return contexts, nil
	}
	changed := false
	for i, m := range gh {
		isURL := false
		for _, k := range serverURLKeys {
			isURL = isURL || m.key == k
		}
		if !isURL {
			continue
		}
		s, ok := str(m.val)
		if !ok || originOf(s) != public {
			continue
		}
		rest := s[strings.Index(s, "://")+3:]
		if k := strings.IndexAny(rest, "/?#"); k >= 0 {
			rest = rest[k:]
		} else {
			rest = ""
		}
		nv, err := marshalNoEscape(target + rest)
		if err != nil {
			return "", err
		}
		gh[i].val = nv
		changed = true
	}
	if !changed {
		return contexts, nil
	}
	if top[gi].val, err = encodeObject(gh); err != nil {
		return "", err
	}
	out, err := encodeObject(top)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// originOf returns "scheme://host[:port]" of an http(s) URL, lower-cased and
// without the default port, an IPv6 host in brackets, or "" for anything else
// (including URLs with credentials, which are never rewritten). It is
// urlorigin.Prefix, the form the executor compares by urlorigin.Canonical.
func originOf(raw string) string { return urlorigin.Prefix(raw) }

func marshalNoEscape(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// parseObject reads a JSON object into its members, in order.
func parseObject(b []byte) ([]jsonMember, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var out []jsonMember
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := kt.(string)
		if !ok {
			return nil, errors.New("not a JSON object")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, jsonMember{k, v})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON object")
	}
	return out, nil
}

func encodeObject(ms []jsonMember) (json.RawMessage, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := marshalNoEscape(m.key)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
