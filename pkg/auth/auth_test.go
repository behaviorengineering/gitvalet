package auth

import "testing"

func TestGitConfigArgsRedactsInTests(t *testing.T) {
	args := GitConfigArgs(Credential{Token: "secret-token", SCM: "github"})
	if len(args) != 2 {
		t.Fatalf("expected 2 args, got %d", len(args))
	}
	if args[1] == "secret-token" {
		t.Fatal("token must not appear in args value")
	}
}

func TestInferSCM(t *testing.T) {
	if InferSCM("https://gitlab.com/foo/bar") != SCMGitLab {
		t.Fatal("expected gitlab")
	}
	if InferSCM("https://github.com/foo/bar") != SCMGitHub {
		t.Fatal("expected github")
	}
}
