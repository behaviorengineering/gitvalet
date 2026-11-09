package gitexec

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRedactArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"plain", []string{"fetch", "--prune", "origin"}, "fetch --prune origin"},
		{"url userinfo", []string{"clone", "https://bot:s3cret@github.com/o/r.git"}, "clone https://***@github.com/o/r.git"},
		{"token only userinfo", []string{"push", "https://x-access-token@github.com/o/r"}, "push https://***@github.com/o/r"},
		{"auth header", []string{"-c", "http.extraHeader=Authorization: Bearer abc.def"}, "-c http.extraHeader=Authorization: ***"},
		{"split auth header", []string{"-H", "Authorization: token abc"}, "-H Authorization: ***"},
		{"private token header", []string{"-H", "PRIVATE-TOKEN: glsecret"}, "-H PRIVATE-TOKEN: ***"},
		{"flag then value", []string{"login", "--token", "abc123"}, "login --token ***"},
		{"flag equals value", []string{"login", "--password=hunter2"}, "login --password=***"},
		{"basic user flag", []string{"-u", "me:pw", "https://api.example.com"}, "-u *** https://api.example.com"},
		{"github token shape", []string{"api", "-f", "x=ghp_abcdefghijklmnopqrstuvwxyz0123"}, "api -f x=***"},
		{"empty", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactArgs(tt.args); got != tt.want {
				t.Fatalf("redactArgs(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestRedactOutput(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"remote url in stderr", "fatal: unable to access 'https://bot:tok@github.com/o/r/': 403", "fatal: unable to access 'https://***@github.com/o/r/': 403"},
		{"glpat in stderr", "error: token glpat-abcdefghijklmnopqrst1 expired", "error: token *** expired"},
		{"github fine-grained", "using github_pat_11ABCDEFGHIJKLMNOPQRSTUV_xyz", "using ***"},
		{"api key pair", "config api_key=abc123 rejected", "config api_key=*** rejected"},
		{"nothing secret", "fatal: not a git repository", "fatal: not a git repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactOutput(tt.in); got != tt.want {
				t.Fatalf("redactOutput(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRunAttemptErrorHasNoSecrets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const secret = "s3cretTok"
	_, err := runAttempt(ctx, 5*time.Second, "git", "ls-remote", "https://bot:"+secret+"@127.0.0.1:1/o/r.git", "--token", secret)
	if err == nil {
		t.Fatal("expected ls-remote against a closed port to fail")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the secret: %v", err)
	}
}
