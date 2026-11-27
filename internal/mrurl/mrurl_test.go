package mrurl

import "testing"

func TestParseMRAcceptsCanonical(t *testing.T) {
	for _, s := range []string{
		"https://gitlab.example.com/acme/shop/-/merge_requests/9",
		"https://gitlab.example.com/acme/sub.group/api/-/merge_requests/31",
		"https://gitlab.com/a_b/c.d/-/merge_requests/1",
	} {
		m, err := ParseMR(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if m.String() != s {
			t.Fatalf("%s: собрано %s", s, m.String())
		}
	}
}

func TestParseMRRejects(t *testing.T) {
	for _, s := range []string{
		"",
		"http://gitlab.example.com/acme/shop/-/merge_requests/9",
		"https://gitlab.example.com/acme/shop/-/merge_requests/9?x=1",
		"https://gitlab.example.com/acme/shop/-/merge_requests/9#note_1",
		"https://gitlab.example.com/acme/shop/-/merge_requests/9/diffs",
		"https://gitlab.example.com/acme/shop/-/merge_requests/09",
		"https://gitlab.example.com/acme/shop/-/merge_requests/0",
		"https://gitlab.example.com/acme/shop/-/merge_requests/9;rm -rf ~",
		"https://gitlab.example.com/acme/shop/-/merge_requests/9&&id",
		"https://gitlab.example.com/acme/shop/-/merge_requests/9 x",
		"https://gitlab.example.com/acme/shop/-/merge_requests/9$(id)",
		"https://gitlab.example.com/acme/shop/-/merge_requests/9`id`",
		"https://gitlab.example.com/acme/shop/-/merge_requests/9\n",
		"https://gitlab.example.com/acme/../etc/-/merge_requests/9",
		"https://gitlab.example.com/acme/./x/-/merge_requests/9",
		"https://gitlab.example.com/ac%20me/x/-/merge_requests/9",
		"https://gitlab.example.com/акме/x/-/merge_requests/9",
		"https://user@gitlab.example.com/acme/shop/-/merge_requests/9",
		"https://gitlab.example.com:8443/acme/shop/-/merge_requests/9",
		"https://GitLab.example.com/acme/shop/-/merge_requests/9",
		"https://gitlab.example.com/Acme/shop/-/merge_requests/9",
		"https://gitlab.example.com/shop/-/merge_requests/9",
		"https://gitlab.example.com/acme/shop/-/issues/9",
		"https://localhost/acme/shop/-/merge_requests/9",
		"https://gitlab.example.com/-x/shop/-/merge_requests/9",
		"https://gitlab.example.com/acme/shop/-/merge_requests/12345678901",
	} {
		if m, err := ParseMR(s); err == nil {
			t.Fatalf("%q принят как %s", s, m)
		}
	}
}

func TestNormalizeMR(t *testing.T) {
	want := "https://gitlab.example.com/acme/shop/-/merge_requests/9"
	for _, s := range []string{
		want,
		"  " + want + "\n",
		want + "/diffs",
		want + "/diffs?commit_id=abc#note_123",
		want + "#note_28554",
		"https://GitLab.Example.com/Acme/Shop/-/merge_requests/9",
	} {
		m, err := NormalizeMR(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if m.String() != want {
			t.Fatalf("%q: %s", s, m)
		}
	}
	for _, s := range []string{
		"https://gitlab.example.com/acme/shop/-/merge_requests/9;id",
		"https://gitlab.example.com/acme/shop",
		"gitlab.example.com/acme/shop/-/merge_requests/9",
	} {
		if m, err := NormalizeMR(s); err == nil {
			t.Fatalf("%q принят как %s", s, m)
		}
	}
}

func TestShort(t *testing.T) {
	m, _ := ParseMR("https://gitlab.example.com/acme/shop/-/merge_requests/9")
	if m.Short() != "shop!9" {
		t.Fatal(m.Short())
	}
}

func TestParseRemoteSameRepo(t *testing.T) {
	want := "gitlab.example.com/acme/api"
	for _, s := range []string{
		"git@gitlab.example.com:acme/api.git",
		"ssh://git@gitlab.example.com:2222/acme/api.git",
		"https://gitlab.example.com/acme/api",
		"https://gitlab.example.com/acme/api.git",
		"https://gitlab.example.com/acme/api/",
		"https://oauth2:secret@gitlab.example.com/acme/api.git",
		"https://gitlab.example.com/acme/api/-/merge_requests/1",
		"https://GitLab.Example.com/Acme/Api",
	} {
		r, err := ParseRemote(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if r.String() != want {
			t.Fatalf("%s: %s", s, r)
		}
	}
	for _, s := range []string{
		"/local/path/repo",
		"file:///tmp/repo",
		"gitlab.example.com",
		"https://gitlab.example.com/onlyone",
		"git@gitlab.example.com:../x/y.git",
	} {
		if r, err := ParseRemote(s); err == nil {
			t.Fatalf("%q принят как %s", s, r)
		}
	}
}

func TestParseRepoKey(t *testing.T) {
	r, err := ParseRepoKey("gitlab.example.com/acme/shop")
	if err != nil || r.Host != "gitlab.example.com" || r.Path != "acme/shop" {
		t.Fatal(r, err)
	}
	for _, s := range []string{"gitlab.example.com", "Gitlab.example.com/a/b", "gitlab.example.com/A/b", "gitlab.example.com/a/../b"} {
		if _, err := ParseRepoKey(s); err == nil {
			t.Fatalf("%q принят", s)
		}
	}
}
