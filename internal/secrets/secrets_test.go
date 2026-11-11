package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, content string) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if content != "" {
		os.WriteFile(path, []byte(content), 0o600)
	}
	s, err := Open(path, filepath.Join(t.TempDir(), "session"), func(b []byte) error { return os.WriteFile(path, b, 0o600) })
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestRegistryKeepsCommentsAndOrder(t *testing.T) {
	s, path := open(t, "# logins\nSHOP_USER=ada@example.com\nexport SHOP_PASSWORD=\"correct horse battery\"\n\n# api\nGITHUB_TOKEN=ghp_abcdefghijklmnop\n")
	ctx := context.Background()
	if v, _ := s.Get(ctx, "SHOP_PASSWORD", ""); v != "correct horse battery" {
		t.Errorf("quoted value = %q", v)
	}
	if err := s.Set("GITHUB_TOKEN", "ghp_new_value_123456"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("CARD", "4111 1111 1111 1111"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("SHOP_USER", ""); err != nil { // delete
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "# logins\nexport SHOP_PASSWORD=\"correct horse battery\"\n\n# api\nGITHUB_TOKEN=ghp_new_value_123456\nCARD=\"4111 1111 1111 1111\"\n"
	if string(got) != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
	if v, _ := s.Get(ctx, "CARD", ""); v != "4111 1111 1111 1111" {
		t.Errorf("round trip through quoting = %q", v)
	}
	var names []string
	for _, n := range s.Names() {
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "SHOP_PASSWORD,GITHUB_TOKEN,CARD" {
		t.Errorf("names = %v", names)
	}
	for _, bad := range []string{"1BAD", "has space", "", "x=y"} {
		if s.Set(bad, "v") == nil {
			t.Errorf("name %q should be rejected", bad)
		}
	}
	if s.Set("MULTI", "a\nb") == nil {
		t.Error("multi-line values should be rejected")
	}
}

func TestAddIsAppendOnly(t *testing.T) {
	s, _ := open(t, "EXISTING=value-123456\n")
	if err := s.Add("NEW_KEY", "generated-abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("EXISTING", "overwrite"); err == nil {
		t.Error("Add must not replace an existing secret")
	}
	for _, name := range Reserved {
		if err := s.Add(name, "x-123456"); err == nil {
			t.Errorf("Add must refuse the reserved name %s", name)
		}
	}
	if v, _ := s.Get(context.Background(), "EXISTING", ""); v != "value-123456" {
		t.Errorf("existing value changed to %q", v)
	}
}

func TestEnvInjectsOnlyNamedNonReservedSecrets(t *testing.T) {
	s, _ := open(t, "GITHUB_TOKEN=ghp_abcdefghijklmnop\nOTHER=other-secret-1\nANTHROPIC_API_KEY=sk-ant-harness-key\n")
	env, used, err := s.Env(context.Background(), `curl -H "Authorization: Bearer $GITHUB_TOKEN" https://x/?k=$ANTHROPIC_API_KEY # GITHUB_TOKENS`, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != "GITHUB_TOKEN=ghp_abcdefghijklmnop" || len(used) != 1 {
		t.Errorf("env = %v, used = %v", env, used)
	}
}

func TestRedact(t *testing.T) {
	s, _ := open(t, "TOKEN=tok_1234567890\nPASSWORD=p@ss word/1\nSHORT=abc\nLONGER=tok_1234567890_extended\n")
	in := "a tok_1234567890 b tok_1234567890_extended c p%40ss+word%2F1 d cEBzcyB3b3JkLzE= e abc"
	got := s.Redact(in)
	want := "a [secret:TOKEN] b [secret:LONGER] c [secret:PASSWORD] d [secret:PASSWORD] e abc"
	if got != want {
		t.Errorf("Redact =\n%s\nwant\n%s", got, want)
	}
	s.Set("TOKEN", "rotated-value-99")
	if got := s.Redact("tok_1234567890 rotated-value-99"); got != "tok_1234567890 [secret:TOKEN]" {
		t.Errorf("after rotation: %s", got)
	}
	if masked := s.Masked(); strings.Contains(masked, "rotated") || !strings.Contains(masked, "TOKEN=********") {
		t.Errorf("Masked leaks: %s", masked)
	}
}

func TestProtonPassReferences(t *testing.T) {
	s, _ := open(t, "DB_PASSWORD=pass://Work/Database/password\nSHOP_TOTP=pass://Work/Shop/totp\n")
	ctx := context.Background()
	if _, err := s.Get(ctx, "DB_PASSWORD", "x"); err == nil || !strings.Contains(err.Error(), "not set up") {
		t.Errorf("without a token the error should say Proton Pass is not set up, got %v", err)
	}
	s.Set(PassTokenName, "pat-token-abcdef")
	var calls []string
	loggedIn := false
	views := 0
	s.Exec = func(_ context.Context, env []string, args ...string) ([]byte, error) {
		joined := strings.Join(env, " ")
		if !strings.Contains(joined, PassTokenName+"=pat-token-abcdef") || !strings.Contains(joined, "PROTON_PASS_SESSION_DIR=") || !strings.Contains(joined, "PROTON_PASS_AGENT_REASON=nightly backup") {
			t.Errorf("pass-cli environment incomplete: %s", joined)
		}
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "info":
			if !loggedIn {
				return nil, errors.New("no session")
			}
		case "login":
			loggedIn = true
		case "item":
			views++
			if strings.HasSuffix(args[2], "/totp") {
				return []byte("12345" + string(rune('0'+views)) + "\n"), nil
			}
			return []byte("s3cret-from-vault\n"), nil
		}
		return nil, nil
	}
	for i := 0; i < 2; i++ {
		if v, err := s.Get(ctx, "DB_PASSWORD", "nightly backup"); err != nil || v != "s3cret-from-vault" {
			t.Fatalf("Get = %q, %v", v, err)
		}
	}
	if got := strings.Join(calls, " | "); got != "info | login | item view pass://Work/Database/password" {
		t.Errorf("pass-cli calls = %s (a value should be fetched once and cached)", got)
	}
	if got := s.Redact("the password is s3cret-from-vault"); got != "the password is [secret:DB_PASSWORD]" {
		t.Errorf("a resolved value must be redacted: %s", got)
	}
	a, _ := s.TOTP(ctx, "SHOP_TOTP", "nightly backup")
	b, _ := s.TOTP(ctx, "SHOP_TOTP", "nightly backup")
	if a == b || len(a) != 6 {
		t.Errorf("one-time codes must never be cached: %q %q", a, b)
	}
	if _, err := s.Use(ctx, PassTokenName, "x"); err == nil {
		t.Error("Use must never hand out the harness's own credentials")
	}
	t.Setenv("FROM_ENVIRONMENT", "env-value-123456")
	if _, err := s.Use(ctx, "FROM_ENVIRONMENT", "x"); err == nil {
		t.Error("Use must not read the process environment")
	}
	if info := s.Names()[0]; info.Reference != "pass://Work/Database/password" {
		t.Errorf("reference not reported: %+v", info)
	}
}

func TestTOTP(t *testing.T) {
	// RFC 6238 test vectors (SHA-1, 8 digits), seed "12345678901234567890".
	seed := "otpauth://totp/x?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&digits=8"
	for unix, want := range map[int64]string{59: "94287082", 1111111109: "07081804", 1234567890: "89005924", 20000000000: "65353130"} {
		if got, err := Code(seed, time.Unix(unix, 0)); err != nil || got != want {
			t.Errorf("Code(%d) = %s, %v; want %s", unix, got, err, want)
		}
	}
	if got, _ := Code("gezd gnbv gy3t qojq gezd gnbv gy3t qojq", time.Unix(59, 0)); got != "287082" {
		t.Errorf("plain base32 seed with spaces: %s", got)
	}
	if _, err := Code("not base32!", time.Now()); err == nil {
		t.Error("an invalid seed should fail")
	}
	s, _ := open(t, "MFA=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ\n")
	if code, err := s.TOTP(context.Background(), "MFA", ""); err != nil || len(code) != 6 {
		t.Errorf("TOTP = %q, %v", code, err)
	}
}
