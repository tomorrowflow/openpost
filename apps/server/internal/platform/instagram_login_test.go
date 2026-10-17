package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const instagramLoginTestProfessionalID = "17841400000000001"

type instagramLoginServer struct {
	*httptest.Server
	codeForm      url.Values
	exchangeQuery url.Values
	refreshQuery  url.Values
	meQuery       url.Values
}

// newInstagramLoginTestAdapter points every Instagram Login host at one test
// server. handlers overrides the response for a request path.
func newInstagramLoginTestAdapter(t *testing.T, handlers map[string]string) (*InstagramLoginAdapter, *instagramLoginServer) {
	t.Helper()
	t.Setenv("META_GRAPH_API_VERSION", "v25.0")
	server := &instagramLoginServer{}
	defaults := map[string]string{
		"/oauth/access_token":   `{"access_token":"short","user_id":99,"permissions":"instagram_business_basic,instagram_business_content_publish"}`,
		"/access_token":         `{"access_token":"long","token_type":"bearer","expires_in":5184000}`,
		"/refresh_access_token": `{"access_token":"refreshed","token_type":"bearer","expires_in":5184000}`,
		"/v25.0/me":             `{"user_id":"` + instagramLoginTestProfessionalID + `","username":"openpost","name":"OpenPost","profile_picture_url":"https://cdn.example/a.jpg","account_type":"BUSINESS"}`,
		"/v25.0/ig-1/media":     `{"id":"container-1"}`,
	}
	for path, body := range handlers {
		defaults[path] = body
	}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := defaults[r.URL.Path]
		if !ok || strings.HasPrefix(body, "status:") {
			status := http.StatusNotFound
			if ok {
				status = http.StatusBadRequest
				body = strings.TrimPrefix(body, "status:")
			} else {
				body = `{"error":{"message":"unexpected path"}}`
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		switch r.URL.Path {
		case "/oauth/access_token":
			_ = r.ParseForm()
			server.codeForm = r.PostForm
		case "/access_token":
			server.exchangeQuery = r.URL.Query()
		case "/refresh_access_token":
			server.refreshQuery = r.URL.Query()
		case "/v25.0/me":
			server.meQuery = r.URL.Query()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	originalClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = originalClient })

	adapter := NewInstagramLoginAdapter("ig-app-id", "ig-secret", "https://app.example/api/v1/accounts/instagram/callback")
	adapter.tokenURL = server.URL + "/oauth/access_token"
	adapter.graphBaseURL = server.URL
	return adapter, server
}

func TestInstagramLoginGenerateAuthURL(t *testing.T) {
	t.Setenv("META_GRAPH_API_VERSION", "v25.0")
	authURL, extra := NewInstagramLoginAdapter("ig-app-id", "ig-secret", "https://app.example/cb").GenerateAuthURL("state-1")
	if extra != nil {
		t.Fatalf("expected no extra auth state, got %#v", extra)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "www.instagram.com" || parsed.Path != "/oauth/authorize" {
		t.Fatalf("unexpected authorize endpoint %s", authURL)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"client_id": "ig-app-id", "redirect_uri": "https://app.example/cb", "response_type": "code",
		"state": "state-1", "enable_fb_login": "false",
		"scope": "instagram_business_basic,instagram_business_content_publish,instagram_business_manage_comments,instagram_business_manage_insights",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestInstagramLoginExchangeCodeAcceptsProviderResponseShapes(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"flat numeric user id and string permissions", `{"access_token":"short","user_id":99,"permissions":"instagram_business_basic,instagram_business_content_publish"}`},
		{"flat string user id and array permissions", `{"access_token":"short","user_id":"99","permissions":["instagram_business_basic","instagram_business_content_publish"]}`},
		{"wrapped numeric user id and string permissions", `{"data":[{"access_token":"short","user_id":99,"permissions":"instagram_business_basic,instagram_business_content_publish"}]}`},
		{"wrapped string user id and array permissions", `{"data":[{"access_token":"short","user_id":"99","permissions":["instagram_business_basic","instagram_business_content_publish"]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, server := newInstagramLoginTestAdapter(t, map[string]string{"/oauth/access_token": test.body})
			token, err := adapter.ExchangeCode(context.Background(), "auth-code", nil)
			if err != nil {
				t.Fatal(err)
			}
			if token.AccessToken != "long" {
				t.Fatalf("expected the long-lived token, got %q", token.AccessToken)
			}
			if token.Extra["scope"] != "instagram_business_basic,instagram_business_content_publish" {
				t.Fatalf("unexpected granted scopes %q", token.Extra["scope"])
			}
			if token.Extra["user_id"] != instagramLoginTestProfessionalID {
				t.Fatalf("account id must be the professional id from /me, got %q", token.Extra["user_id"])
			}
			for key, want := range map[string]string{
				"client_id": "ig-app-id", "client_secret": "ig-secret", "grant_type": "authorization_code",
				"redirect_uri": "https://app.example/api/v1/accounts/instagram/callback", "code": "auth-code",
			} {
				if got := server.codeForm.Get(key); got != want {
					t.Errorf("code exchange %s = %q, want %q", key, got, want)
				}
			}
			if server.exchangeQuery.Get("grant_type") != "ig_exchange_token" ||
				server.exchangeQuery.Get("client_secret") != "ig-secret" ||
				server.exchangeQuery.Get("access_token") != "short" {
				t.Fatalf("unexpected long-lived exchange query %v", server.exchangeQuery)
			}
			if server.meQuery.Get("access_token") != "long" {
				t.Fatalf("/me must be read with the long-lived token, got %v", server.meQuery)
			}
		})
	}
}

func TestInstagramLoginExchangeCodeReportsRefreshableExpiry(t *testing.T) {
	const fiftyDays = 50 * 24 * 60 * 60
	for _, test := range []struct {
		name      string
		expiresIn string
		want      int
	}{
		{"sixty day token is reported at fifty days", `5184000`, fiftyDays},
		{"missing expiry is reported at fifty days", `0`, fiftyDays},
		{"shorter provider expiry is kept", `3600`, 3600},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, _ := newInstagramLoginTestAdapter(t, map[string]string{
				"/access_token": `{"access_token":"long","token_type":"bearer","expires_in":` + test.expiresIn + `}`,
			})
			token, err := adapter.ExchangeCode(context.Background(), "auth-code", nil)
			if err != nil {
				t.Fatal(err)
			}
			if token.ExpiresIn != test.want {
				t.Fatalf("ExpiresIn = %d, want %d", token.ExpiresIn, test.want)
			}
		})
	}
}

func TestInstagramLoginExchangeCodeFailsWhenLongLivedExchangeFails(t *testing.T) {
	adapter, _ := newInstagramLoginTestAdapter(t, map[string]string{
		"/access_token": `status:{"error":{"message":"denied","code":190}}`,
	})
	token, err := adapter.ExchangeCode(context.Background(), "auth-code", nil)
	if err == nil || token != nil {
		t.Fatalf("a failed long-lived exchange must not fall back to the short-lived token, got token=%v err=%v", token, err)
	}
}

func TestInstagramLoginExchangeCodeRejectsMissingAccessToken(t *testing.T) {
	adapter, _ := newInstagramLoginTestAdapter(t, map[string]string{"/oauth/access_token": `{"data":[]}`})
	if _, err := adapter.ExchangeCode(context.Background(), "auth-code", nil); err == nil {
		t.Fatal("expected an error when the code exchange returns no access token")
	}
}

func TestInstagramLoginRefreshToken(t *testing.T) {
	adapter, server := newInstagramLoginTestAdapter(t, nil)
	capability := adapter.RefreshCapability()
	if !capability.Supported || capability.CredentialSource != RefreshCredentialAccessToken {
		t.Fatalf("unexpected refresh capability %#v", capability)
	}
	token, err := adapter.RefreshToken(context.Background(), RefreshTokenInput{AccessToken: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "refreshed" || token.ExpiresIn != 50*24*60*60 {
		t.Fatalf("unexpected refreshed token %#v", token)
	}
	if len(token.Extra) != 0 {
		t.Fatalf("refresh must leave granted scopes untouched, got %#v", token.Extra)
	}
	if server.refreshQuery.Get("grant_type") != "ig_refresh_token" || server.refreshQuery.Get("access_token") != "old" {
		t.Fatalf("unexpected refresh query %v", server.refreshQuery)
	}
	if _, err := adapter.RefreshToken(context.Background(), RefreshTokenInput{}); err == nil {
		t.Fatal("refresh without an access token must fail")
	}
}

func TestInstagramLoginRefreshKeepsShorterProviderExpiry(t *testing.T) {
	adapter, _ := newInstagramLoginTestAdapter(t, map[string]string{
		"/refresh_access_token": `{"access_token":"refreshed","expires_in":7200}`,
	})
	token, err := adapter.RefreshToken(context.Background(), RefreshTokenInput{AccessToken: "old"})
	if err != nil || token.ExpiresIn != 7200 {
		t.Fatalf("token=%#v err=%v", token, err)
	}
}

func TestInstagramLoginGetProfile(t *testing.T) {
	adapter, server := newInstagramLoginTestAdapter(t, map[string]string{
		"/v25.0/me": `{"user_id":17841400000000001,"username":"openpost","name":"OpenPost","profile_picture_url":"https://cdn.example/a.jpg","account_type":"BUSINESS"}`,
	})
	profile, err := adapter.GetProfile(context.Background(), "long")
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != instagramLoginTestProfessionalID || profile.Username != "openpost" || profile.DisplayName != "OpenPost" || profile.AvatarURL != "https://cdn.example/a.jpg" {
		t.Fatalf("unexpected profile %#v", profile)
	}
	if len(profile.CapabilityState) != 1 || profile.CapabilityState["connection_type"] != "instagram_login" {
		t.Fatalf("capability state must carry only the connection type, got %#v", profile.CapabilityState)
	}
	if fields := server.meQuery.Get("fields"); fields != "user_id,username,name,profile_picture_url,account_type" {
		t.Fatalf("unexpected /me fields %q", fields)
	}
}

func TestInstagramLoginAccountSelectionKeepsTheTokenUnchanged(t *testing.T) {
	adapter, _ := newInstagramLoginTestAdapter(t, nil)
	token := &TokenResult{AccessToken: "long", ExpiresIn: 4320000, TokenType: "Bearer", Extra: map[string]string{"user_id": instagramLoginTestProfessionalID, "scope": "instagram_business_basic"}}

	options, err := adapter.ListAccountSelections(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || options[0].ID != instagramLoginTestProfessionalID || options[0].Username != "openpost" {
		t.Fatalf("expected exactly the /me account, got %#v", options)
	}

	selected, err := adapter.SelectAccount(context.Background(), token, options[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if selected.AccountID != instagramLoginTestProfessionalID || selected.AccountUsername != "openpost" {
		t.Fatalf("unexpected selected account %#v", selected)
	}
	if selected.Token.AccessToken != "long" || selected.Token.ExpiresIn != 4320000 ||
		selected.Token.Extra["user_id"] != instagramLoginTestProfessionalID || selected.Token.Extra["scope"] != "instagram_business_basic" {
		t.Fatalf("selection must not alter the token, got %#v", selected.Token)
	}
	if len(selected.CapabilityState) != 1 || selected.CapabilityState["connection_type"] != "instagram_login" {
		t.Fatalf("unexpected capability state %#v", selected.CapabilityState)
	}
	if _, err := adapter.SelectAccount(context.Background(), token, "someone-else"); err == nil {
		t.Fatal("selecting an account other than the authorized one must fail")
	}
}

func TestInstagramLoginAdapterIsAnAccountSelectionAdapter(t *testing.T) {
	var adapter Adapter = NewInstagramLoginAdapter("id", "secret", "https://app.example/cb")
	if _, ok := adapter.(AccountSelectionAdapter); !ok {
		t.Fatal("the OAuth callback relies on the account selection step")
	}
}

func TestInstagramLoginPublishContainerUsesInstagramGraphHost(t *testing.T) {
	t.Setenv("META_GRAPH_API_VERSION", "v25.0")
	adapter := NewInstagramLoginAdapter("ig-app-id", "ig-secret", "https://app.example/cb")
	var seen *http.Request
	var form url.Values
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = w.Write([]byte(`{"id":"container-1"}`))
	}))
	t.Cleanup(graph.Close)
	adapter.graphBaseURL = graph.URL
	originalClient := httpClient
	httpClient = graph.Client()
	t.Cleanup(func() { httpClient = originalClient })

	id, err := adapter.createMediaContainer(context.Background(), "long", "ig-1", "Caption", "https://media.example/a.jpg", false, false, &PublishRequest{}, nil, "")
	if err != nil || id != "container-1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if seen == nil || seen.URL.Path != "/v25.0/ig-1/media" || form.Get("image_url") != "https://media.example/a.jpg" || form.Get("access_token") != "long" {
		t.Fatalf("container must be created on the Instagram Graph base, got %v %v", seen, form)
	}
	if got := NewInstagramLoginAdapter("", "", "").graphURL("ig-1/media"); got != "https://graph.instagram.com/v25.0/ig-1/media" {
		t.Fatalf("default graph URL %q", got)
	}
}

func TestInstagramLoginCapabilitiesUseBusinessScopes(t *testing.T) {
	t.Setenv("META_GRAPH_API_VERSION", "v25.0")
	adapter := NewInstagramLoginAdapter("id", "secret", "https://app.example/cb")

	if messaging := adapter.MessagingSupport(); messaging.Enabled || messaging.CanSend || messaging.Unavailable == "" {
		t.Fatalf("messaging must be unavailable with an explanation, got %#v", messaging)
	}
	if engagement := adapter.EngagementSupport(); !engagement.Enabled || strings.Join(engagement.RequiredScopes, ",") != "instagram_business_manage_comments" {
		t.Fatalf("unexpected engagement support %#v", engagement)
	}
	if analytics := adapter.AnalyticsSupport(); !analytics.Account || !analytics.Content || strings.Join(analytics.ContentRequiredScopes, ",") != "instagram_business_manage_insights" {
		t.Fatalf("unexpected analytics support %#v", analytics)
	}
	if native := adapter.NativePostSupport(); !native.Supported || strings.Join(native.RequiredScopes, ",") != "instagram_business_basic" {
		t.Fatalf("unexpected native post support %#v", native)
	}
	page, err := adapter.SearchPublishingOptions(context.Background(), "token", PublishingOptionsInput{Source: "instagram_locations", Search: "berlin"})
	if err != nil || len(page.Options) != 0 {
		t.Fatalf("location search must return no options without calling Facebook, got %#v err=%v", page, err)
	}
	if descriptor := adapter.AuthorizationGrantDescriptor(); descriptor.Evidence["connection_type"] != "instagram_login" || descriptor.ProjectID != "id" {
		t.Fatalf("unexpected grant descriptor %#v", descriptor)
	}
}
