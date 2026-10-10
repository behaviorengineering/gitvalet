// Package auth maps forge credentials to safe Git HTTPS configuration.
package auth

import (
	"encoding/base64"
	"strings"
)

// SCM name constants for Credential.SCM and InferSCM.
const (
	SCMGitHub    = "github"
	SCMGitLab    = "gitlab"
	SCMBitbucket = "bitbucket"
)

// Credential is a forge-scoped HTTPS token for Git smart HTTP.
type Credential struct {
	Token string
	SCM   string // github, gitlab, bitbucket, or empty for GitHub-style default
}

// Build authenticated Git HTTPS header args for http.extraHeader. Empty token yields nil.
func GitConfigArgs(cred Credential) []string {
	token := strings.TrimSpace(cred.Token)
	if token == "" {
		return nil
	}
	scm := strings.ToLower(strings.TrimSpace(cred.SCM))
	var header string
	switch scm {
	case SCMGitLab:
		basic := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
		header = "Authorization: Basic " + basic
	case SCMBitbucket:
		basic := base64.StdEncoding.EncodeToString([]byte("x-token-auth:" + token))
		header = "Authorization: Basic " + basic
	default:
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		header = "Authorization: Basic " + basic
	}
	return []string{"-c", "http.extraHeader=" + header}
}

// InferSCM guesses the forge from an HTTPS remote URL host.
func InferSCM(remoteURL string) string {
	u := strings.ToLower(remoteURL)
	switch {
	case strings.Contains(u, SCMGitLab):
		return SCMGitLab
	case strings.Contains(u, SCMBitbucket):
		return SCMBitbucket
	default:
		return SCMGitHub
	}
}

// RedactToken returns a safe placeholder when token is non-empty.
func RedactToken(token string) string {
	if strings.TrimSpace(token) == "" {
		return ""
	}
	return "<redacted>"
}
