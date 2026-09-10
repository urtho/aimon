package providers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
	"github.com/algonode/aimon/internal/oauth"
	"github.com/algonode/aimon/internal/store"
)

const openAITestUsage = `{
	"plan_type":"plus",
	"rate_limit":{
		"allowed":true,"limit_reached":false,
		"primary_window":{"used_percent":0.8,"limit_window_seconds":18000,"reset_at":1800003600},
		"secondary_window":{"used_percent":32.5,"limit_window_seconds":604800,"reset_at":1800600000}
	}
}`

func TestParseOpenAIUsage(t *testing.T) {
	now := time.Unix(1800000000, 0)
	st, err := parseOpenAIUsage("My OpenAI", []byte(openAITestUsage), now)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.Provider != "openai" || st.Label != "My OpenAI" || st.Kind != model.KindSubscription || st.Unit != "%" || !strings.Contains(st.Plan, "plus") {
		t.Fatalf("unexpected status: %+v", st)
	}
	want := []model.Span{
		{Name: "5h", Used: 0.8, ResetAt: 1800003600, Duration: 18000},
		{Name: "1w", Used: 32.5, ResetAt: 1800600000, Duration: 604800},
	}
	if !reflect.DeepEqual(st.Spans, want) {
		t.Fatalf("spans = %+v, want %+v", st.Spans, want)
	}
	if st.Balance == nil || *st.Balance != 67.5 || st.Used == nil || *st.Used != 32.5 || st.Total == nil || *st.Total != 100 || st.ResetAt == nil || *st.ResetAt != 1800600000 {
		t.Fatalf("aggregate must track the tightest window: %+v", st)
	}
}

func TestParseOpenAIUsageWindows(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, tc := range []struct {
		name string
		raw  string
		want []model.Span
	}{
		{"zero used", `{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":60},"secondary_window":null}}`, []model.Span{{Name: "5h", Used: 0, ResetAt: now.Unix() + 60, Duration: 18000}}},
		{"secondary only", `{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":90,"limit_window_seconds":86400}}}`, []model.Span{{Name: "1d", Used: 90, Duration: 86400}}},
		{"clamp and missing duration", `{"rate_limit":{"primary_window":{"used_percent":-2},"secondary_window":{"used_percent":120}}}`, []model.Span{{Name: "primary", Used: 0}, {Name: "secondary", Used: 100}}},
		{"absolute reset wins", `{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":900,"reset_at":1800000300,"reset_after_seconds":60}}}`, []model.Span{{Name: "15m", Used: 12, ResetAt: now.Unix() + 300, Duration: 900}}},
		{"reset now", `{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":1800,"reset_after_seconds":0}}}`, []model.Span{{Name: "30m", Used: 12, ResetAt: now.Unix(), Duration: 1800}}},
		{"invalid reset", `{"rate_limit":{"primary_window":{"used_percent":12,"reset_at":-1,"reset_after_seconds":-1}}}`, []model.Span{{Name: "primary", Used: 12}}},
		{"missing usage skipped", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000},"secondary_window":{"used_percent":0,"limit_window_seconds":604800}}}`, []model.Span{{Name: "1w", Used: 0, Duration: 604800}}},
		{"additional and review", `{"code_review_rate_limit":{"primary_window":{"used_percent":8,"limit_window_seconds":604800}},"additional_rate_limits":[{"limit_name":"Spark","rate_limit":{"primary_window":{"used_percent":42,"limit_window_seconds":18000}}},{"metered_feature":"other","rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":60}}}]}`, []model.Span{{Name: "1w review", Used: 8, Duration: 604800}, {Name: "5h Spark", Used: 42, Duration: 18000}, {Name: "1m other", Used: 5, Duration: 60}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := parseOpenAIUsage("OpenAI", []byte(tc.raw), now)
			if err != nil || !st.OK || !reflect.DeepEqual(st.Spans, tc.want) {
				t.Fatalf("spans = %+v, error = %v; want %+v", st.Spans, err, tc.want)
			}
		})
	}
}

func TestParseOpenAIUsageRejectsMissingOrMalformedUsage(t *testing.T) {
	for _, raw := range []string{"", "{", "null", "{}", `{"plan_type":"pro","rate_limit":null}`, `{"rate_limit":{"primary_window":{"used_percent":null}}}`, `{"rate_limit":{"primary_window":{"used_percent":"invalid"}}}`} {
		st, err := parseOpenAIUsage("OpenAI", []byte(raw), time.Now())
		if err == nil || st.OK || st.Balance != nil || len(st.Spans) != 0 {
			t.Fatalf("must not fabricate usage for %q: %+v, %v", raw, st, err)
		}
	}
}

func openAITestJWT(account string) string {
	raw, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".test"
}

func TestReadCodexCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, raw, token, account string
	}{
		{"nested", `{"OPENAI_API_KEY":null,"tokens":{"access_token":"access","account_id":"workspace","refresh_token":"ignored","id_token":"ignored"}}`, "access", "workspace"},
		{"flat", `{"access_token":"access","account_id":"workspace"}`, "access", "workspace"},
		{"access claims", fmt.Sprintf(`{"tokens":{"access_token":%q}}`, openAITestJWT("access-account")), openAITestJWT("access-account"), "access-account"},
		{"id claims", fmt.Sprintf(`{"tokens":{"access_token":"opaque","id_token":%q}}`, openAITestJWT("id-account")), "opaque", "id-account"},
		{"opaque", `{"tokens":{"access_token":"opaque"}}`, "opaque", ""},
		{"api key only", `{"OPENAI_API_KEY":"sk-test","tokens":null}`, "", ""},
		{"refresh only", `{"tokens":{"refresh_token":"must-not-be-used"}}`, "", ""},
		{"malformed", `{"tokens":`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			token, account, err := readCodexCredentials(path)
			if tc.token == "" {
				if err == nil {
					t.Fatal("expected credentials error")
				}
				return
			}
			if err != nil || token != tc.token || account != tc.account {
				t.Fatalf("token=%q account=%q error=%v", token, account, err)
			}
		})
	}
	for _, token := range []string{"", "opaque", "a.!!!.b", "a.bm90LWpzb24.b"} {
		if got := openAIAccountID(token); got != "" {
			t.Fatalf("unexpected account from invalid token: %q", got)
		}
	}
}

func TestOpenAIFetchCredentialsAndFileReload(t *testing.T) {
	var gotAuth, gotAccount, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAccount, gotPath = r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id"), r.URL.Path
		if r.Method != http.MethodGet || r.Header.Get("Accept") != "application/json" {
			t.Errorf("unexpected usage request: %s, accept=%q", r.Method, r.Header.Get("Accept"))
		}
		fmt.Fprint(w, openAITestUsage)
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "auth.json")
	o := newOpenAI(config.Monitor{Label: "OpenAI", TokenFile: path, BearerToken: "ignored", BaseURL: upstream.URL + "/backend-api/"}, nil)
	for _, token := range []string{"first", "rotated"} {
		raw := fmt.Sprintf(`{"tokens":{"access_token":%q,"account_id":"file-account"}}`, token)
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := o.Fetch(context.Background())
		if err != nil || !st.OK || gotAuth != "Bearer "+token || gotAccount != "file-account" || gotPath != "/backend-api/wham/usage" {
			t.Fatalf("fetch=%+v err=%v auth=%q account=%q path=%q", st, err, gotAuth, gotAccount, gotPath)
		}
		data, _ := os.ReadFile(path)
		if string(data) != raw {
			t.Fatal("aimon must not modify externally managed credentials")
		}
		encoded, _ := json.Marshal(st)
		if strings.Contains(string(encoded), token) || strings.Contains(string(encoded), "file-account") {
			t.Fatal("status exposes credentials or account routing data")
		}
	}
	o.tokenFile = ""
	o.bearer = openAITestJWT("jwt-account")
	if _, err := o.Fetch(context.Background()); err != nil || gotAccount != "jwt-account" {
		t.Fatalf("JWT account routing: account=%q err=%v", gotAccount, err)
	}
	o.accountID = "override"
	o.usageURL = upstream.URL + "/custom"
	if _, err := o.Fetch(context.Background()); err != nil || gotAccount != "override" || gotPath != "/custom" {
		t.Fatalf("overrides: account=%q path=%q err=%v", gotAccount, gotPath, err)
	}
}

func TestOpenAIFetchAuthAndHTTPFailures(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aimon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mgr := oauth.New(db)
	ctx := context.Background()
	for _, manager := range []*oauth.Manager{nil, mgr} {
		o := newOpenAI(config.Monitor{Label: "OpenAI", APIKey: "sk-not-subscription"}, manager)
		st, err := o.Fetch(ctx)
		if err != nil || st.OK || !st.NeedsAuth {
			t.Fatalf("missing subscription credentials: %+v, %v", st, err)
		}
	}
	if err := db.SaveToken(ctx, store.Token{Provider: "openai", Label: "OpenAI", AccessToken: "stored"}); err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				fmt.Fprint(w, "<html>private upstream body</html>")
			}))
			defer upstream.Close()
			for _, bearer := range []string{"", "static"} {
				o := newOpenAI(config.Monitor{Label: "OpenAI", UsageURL: upstream.URL, BearerToken: bearer}, mgr)
				st, err := o.Fetch(ctx)
				wantAuth := bearer == "" && (code == 401 || code == 403)
				if err == nil || st.OK || st.NeedsAuth != wantAuth || strings.Contains(err.Error(), "private upstream") {
					t.Fatalf("HTTP %d: %+v, %v", code, st, err)
				}
			}
		})
	}
	o := newOpenAI(config.Monitor{TokenFile: filepath.Join(t.TempDir(), "missing.json"), BearerToken: "fallback-must-not-be-used"}, mgr)
	if st, err := o.Fetch(ctx); err == nil || st.NeedsAuth {
		t.Fatalf("missing external file must report an error without Connect: %+v, %v", st, err)
	}
}

func TestOpenAIOAuthConnectRefreshAndFetch(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "aimon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mgr := oauth.New(db)
	o := newOpenAI(config.Monitor{Label: "OpenAI"}, mgr)
	cfg := o.OAuthConfig()
	authURL, state, err := mgr.StartFlow(ctx, o.Label(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	if q.Get("redirect_uri") != openAIRedirectURI || q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "offline_access") {
		t.Fatalf("invalid OAuth authorization parameters: %v", q)
	}
	var grants []string
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		grant := r.PostForm.Get("grant_type")
		grants = append(grants, grant)
		if r.Method != http.MethodPost || r.PostForm.Get("client_id") != openAIClientID {
			t.Error("invalid token request")
		}
		if grant == "authorization_code" {
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") || r.PostForm.Get("code") != "test-code" || r.PostForm.Get("redirect_uri") != openAIRedirectURI {
				t.Error("incorrect PKCE exchange or pasted callback parsing")
			}
			fmt.Fprint(w, `{"access_token":"initial","refresh_token":"refresh","expires_in":1}`)
		} else {
			if grant != "refresh_token" || r.PostForm.Get("refresh_token") != "refresh" {
				t.Error("invalid refresh grant")
			}
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, openAITestJWT("oauth-account"))
		}
	}))
	defer tokenEndpoint.Close()
	cfg.TokenURL = tokenEndpoint.URL
	if err := mgr.Complete(ctx, cfg, state, openAIRedirectURI+"?code=test-code&state="+state); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AccessToken(ctx, o.Label(), cfg); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetToken(ctx, o.ID(), o.Label())
	if err != nil || stored.RefreshToken != "refresh" || !reflect.DeepEqual(grants, []string{"authorization_code", "refresh_token"}) {
		t.Fatalf("refresh persistence: grants=%v err=%v", grants, err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+stored.AccessToken || r.Header.Get("ChatGPT-Account-Id") != "oauth-account" {
			t.Error("stored OAuth token or account missing from usage request")
		}
		fmt.Fprint(w, openAITestUsage)
	}))
	defer upstream.Close()
	o.usageURL = upstream.URL
	if st, err := o.Fetch(ctx); err != nil || !st.OK {
		t.Fatalf("connected usage: %+v, %v", st, err)
	}
	if err := mgr.Disconnect(ctx, o.ID(), o.Label()); err != nil {
		t.Fatal(err)
	}
	if st, err := o.Fetch(ctx); err != nil || !st.NeedsAuth {
		t.Fatalf("disconnect: %+v, %v", st, err)
	}
}
